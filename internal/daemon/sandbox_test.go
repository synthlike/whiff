package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/sandbox"
)

// TestSandboxJob runs :sandbox jobs: bad options are refused anywhere;
// where there's a sandbox, the job runs in Linux, writes only its dir, and
// a cancel ends its container; where there isn't, it fails to start.
func TestSandboxJob(t *testing.T) {
	oneContainerAtATime(t)
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{})
	start(t, dir)
	c := dial(t, dir)
	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, dir)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return resp.Value + resp.Error
	}
	for src, want := range map[string]string{
		`(call :jobs/run ["true"] {:sandbox {:net 1}})`:                 "want :sandbox #t, or {:net #t}",
		`(call :jobs/run ["true"] {:sandbox #t :session [:todo/open]})`: ":session and :sandbox don't go together",
	} {
		if got := eval(src); !strings.Contains(got, want) {
			t.Errorf("%s = %s, want it to say %q", src, got, want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := sandbox.New(ctx, false); err != nil {
		got := eval(`(call :jobs/run ["true"] {:sandbox #t})`)
		if !strings.Contains(got, ":kind :start") {
			t.Errorf("without a sandbox, a :sandbox job = %s; want it not to start", got)
		}
		t.Skipf("the rest needs a sandbox: %v", err)
	}
	wait := func(id string) string {
		t.Helper()
		resp, err := c.Message("wait", "", "["+id+"]", "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	pending := func(src string) string {
		t.Helper()
		return eval(`(match ` + src + ` [(err {:kind :pending :id id}) id])`)
	}
	id := pending(`(call :jobs/run ["sh" "-c" "echo boxed > f; uname -s"] {:sandbox #t :as :text})`)
	if got := wait(id); got != `(ok "Linux\n")` {
		t.Errorf("a sandboxed job = %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "f")); err != nil || string(b) != "boxed\n" {
		t.Errorf("its dir has %q %v", b, err)
	}

	id = pending(`(call :jobs/run ["sleep" "300"] {:sandbox #t})`)
	list := eval(`(match (call :jobs/list) [(ok [{:container n}]) n])`)
	box := strings.Trim(list, `"`)
	if !strings.HasPrefix(box, "whiff-job-") {
		t.Fatalf(":jobs/list's container = %s", list)
	}
	host, err := sandbox.Host()
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "its container", func() string { return strconv.FormatBool(host.Gone(ctx, box)) }, "false")
	eval(`(call :jobs/cancel ` + id + `)`)
	if got := wait(id); !strings.Contains(got, ":kind :cancelled") {
		t.Errorf("a cancelled sandboxed job = %s", got)
	}
	if !host.Gone(ctx, box) {
		host.End(ctx, box)
		t.Errorf("container %s outlived its cancelled job", box)
	}
}

// TestSandboxStray: a sandboxed job's container that still runs after
// whiff died is ended at start, as its name, unlike a pid, can't be
// another's; the job is then lost as one that runs no more.
func TestSandboxStray(t *testing.T) {
	oneContainerAtATime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := sandbox.New(ctx, false)
	if err != nil {
		t.Skip(err)
	}
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"waiter": `(service waiter
  (on (whiff/done id r) "Notes the answer." (file/append "` + got + `" (string/concat (int/to-string id) " " (value/to-string r) "\n"))))`,
	})
	done := start(t, dir)
	cl := dial(t, dir)
	stopDaemon(t, cl, done)

	// What whiff dying leaves: the job stored as running, its client and
	// container still there.
	box := sandbox.Name("job")
	client := c.Command(ctx, box, dir, []string{"sleep", "300"})
	client.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { client.Wait(); close(reaped) }()
	t.Cleanup(func() { c.End(context.Background(), box); <-reaped })
	eventually(t, "the container", func() string { return strconv.FormatBool(c.Gone(ctx, box)) }, "false")
	j := &job{ticket: ticket{id: 900, from: "waiter", user: true}, cmd: puff.NewVector(puff.String("sleep")),
		pid: client.Process.Pid, box: box, opts: jobOpts{sandbox: true}}
	j.attempts = []attempt{{started: time.Now(), pid: j.pid, box: box}}
	withDB(t, dir, func(db *bbolt.DB) {
		err := db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(tasksBucket).Put(idKey(j.id), []byte(puff.Print(j.stored())))
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	start(t, dir)
	cl = dial(t, dir)
	eventually(t, "the lost job's answer", func() string { b, _ := os.ReadFile(got); return string(b) },
		`900 (err {:kind :interrupted :message "job 900 was running when whiff died: how far it got is unknown" :outcome :unknown})`+"\n")
	if !c.Gone(ctx, box) {
		t.Errorf("container %s still runs", box)
	}
	historyHas(t, cl, "job 900's container "+box+" still ran after whiff died, so whiff ended it")
}

// TestPipelineSandbox runs a sandboxed pipeline run with its dev on huff
// and a fake model: the dev's Bash and the check both answer Linux, as a
// container does on any host, git works in the run's worktree for both,
// and the run ends ready.
func TestPipelineSandbox(t *testing.T) {
	oneContainerAtATime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if _, err := sandbox.New(ctx, false); err != nil {
		t.Skip(err)
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	// withHuff moves HOME; rootless Podman keeps its images under it.
	home, _ := os.UserHomeDir()
	t.Setenv("XDG_DATA_HOME", cmp.Or(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")))
	t.Setenv("XDG_CONFIG_HOME", cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")))
	dir := tempDir(t)
	withHuff(t, dir)

	// The model: a dev (it has tools) writes a file and runs uname -s and
	// git status, then says so; a reviewer approves.
	var mu sync.Mutex
	var bashSaid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string
				Content *string
			}
			Tools []struct{ Function struct{ Name string } }
		}
		json.Unmarshal(b, &req)
		last := req.Messages[len(req.Messages)-1]
		reply := func(content string) {
			text, _ := json.Marshal(content)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"cost":0.001}}`)
		}
		switch {
		case len(req.Tools) == 0:
			reply("Fine.\nSUBJECT: Add f.txt\nVERDICT: approve")
		case last.Role == "user":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"f.txt\",\"content\":\"x\\n\"}"}},
				{"id":"c2","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"sh -c uname\"}"}},
				{"id":"c3","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"git status --short\"}"}}]},
				"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"cost":0.001}}`)
		default:
			mu.Lock()
			bashSaid = ""
			for _, m := range req.Messages {
				if m.Role == "tool" && m.Content != nil {
					bashSaid += *m.Content + "\n"
				}
			}
			mu.Unlock()
			reply("Wrote f.txt.")
		}
	}))
	defer srv.Close()
	t.Setenv("WHIFF_LOCAL_URL", srv.URL)

	writeServices(t, dir, map[string]string{"pipeline": string(src)})
	start(t, dir)
	c := dial(t, dir)
	if _, err := c.Sub("pipeline/done"); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}

	// Without huff, the run can't be sandboxed.
	resp, err := c.Message("call", "pipeline/start-on", `["`+repo+`" "main" "add f.txt" ["uname" "-s"] {:dev "haiku" :sandbox #t}]`, repo)
	if err != nil || !strings.Contains(resp.Value, ":no-sandbox") {
		t.Errorf("a sandboxed run on claude = %s %v", resp.Value, err)
	}
	check := `["sh" "-c" "uname -s && git rev-parse --abbrev-ref HEAD"]`
	resp, err = c.Message("call", "pipeline/start-on", `["`+repo+`" "main" "add f.txt" `+check+` {:dev "local/fake" :reviewer "local/fake" :sandbox #t}]`, repo)
	if err != nil || resp.Value != "(ok 1)" {
		t.Fatalf("start-on = %s %s %v", resp.Value, resp.Error, err)
	}
	select {
	case e := <-c.Events():
		if !strings.Contains(e.Value, ":status :ready") {
			t.Errorf("done = %s", e.Value)
		}
	case <-time.After(2 * time.Minute):
		h, _ := historyOf(c)
		t.Fatalf("no :pipeline/done; history:\n%s", h)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(bashSaid, "Linux") {
		t.Errorf("the dev's Bash said %q, want Linux, from a container", bashSaid)
	}
	if !strings.Contains(bashSaid, "?? f.txt") {
		t.Errorf("the dev's git status said %q, want f.txt untracked", bashSaid)
	}
	// The check ran in the container, its output kept in its file too.
	historyHas(t, c, `/jobs/`)
	historyHas(t, c, `" :stderr "" :stdout "Linux\nwhiff/1\n"})`)
}

// TestPipelineGitRedirect has a dev, with its file tools only, plant a
// .git in its worktree that names a gitdir of its own, whose config runs
// a command when git looks at the tree. The pipeline runs git on the host
// there after the dev's turn (add, diff, commit), so if git followed the
// planted .git, the dev's command would run unconfined, sandbox or not.
// The pipeline's git uses the worktree's real gitdir instead: the command
// never runs, and the planted files are only the dev's work.
func TestPipelineGitRedirect(t *testing.T) {
	oneContainerAtATime(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	withHuff(t, dir)
	marker := filepath.Join(dir, "PWNED")

	// The dev writes the gitdir and the .git naming it; a reviewer approves.
	files := [][2]string{
		{"evil/HEAD", "ref: refs/heads/main\n"},
		{"evil/objects/info/keep", ""},
		{"evil/refs/heads/keep", ""},
		{"evil/config", "[core]\n\trepositoryformatversion = 0\n\tfsmonitor = touch " + marker + "\n"},
		{".git", "gitdir: evil\n"},
	}
	var calls []string
	for i, f := range files {
		args, _ := json.Marshal(map[string]string{"file_path": f[0], "content": f[1]})
		call, _ := json.Marshal(map[string]any{"id": "c" + strconv.Itoa(i), "type": "function",
			"function": map[string]string{"name": "Write", "arguments": string(args)}})
		calls = append(calls, string(call))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role string }
			Tools    []struct{ Function struct{ Name string } }
		}
		json.Unmarshal(b, &req)
		reply := func(content string) {
			text, _ := json.Marshal(content)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"cost":0.001}}`)
		}
		switch {
		case len(req.Tools) == 0:
			reply("Fine.\nSUBJECT: Add files\nVERDICT: approve")
		case req.Messages[len(req.Messages)-1].Role == "user":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[`+strings.Join(calls, ",")+
				`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"cost":0.001}}`)
		default:
			reply("Done.")
		}
	}))
	defer srv.Close()
	t.Setenv("WHIFF_LOCAL_URL", srv.URL)

	writeServices(t, dir, map[string]string{"pipeline": string(src)})
	start(t, dir)
	c := dial(t, dir)
	if _, err := c.Sub("pipeline/done"); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}

	resp, err := c.Message("call", "pipeline/start-on", `["`+repo+`" "main" "add files" ["true"] {:dev "local/fake" :reviewer "local/fake"}]`, repo)
	if err != nil || resp.Value != "(ok 1)" {
		t.Fatalf("start-on = %s %s %v", resp.Value, resp.Error, err)
	}
	select {
	case e := <-c.Events():
		if !strings.Contains(e.Value, ":status :ready") {
			t.Errorf("done = %s", e.Value)
		}
	case <-time.After(2 * time.Minute):
		h, _ := historyOf(c)
		t.Fatalf("no :pipeline/done; history:\n%s", h)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the pipeline's git ran a command from the dev's planted gitdir")
	}
	// The planted gitdir is committed as the dev's work, for review.
	out, err := exec.Command("git", "-C", repo, "show", "--stat", "--format=", "whiff/1").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "evil/config") {
		t.Errorf("whiff/1's commit = %s %v", out, err)
	}
}

// oneContainerAtATime holds a lock on this machine for the rest of the
// test, shared by every whiff package's tests that boot a container:
// Apple's runtime fails to bootstrap two at once, and go test runs
// packages side by side.
func oneContainerAtATime(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "whiff-container-tests.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}
