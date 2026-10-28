package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// TestGitService tries services/git.puff, as whiff try does, in a scratch
// repository.
func TestGitService(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "git.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)

	repo := filepath.Join(dir, "repo")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(repo, 0o700)
	git("init", "-q", "-b", "main")
	write("a.txt", "a")
	write("old name.txt", "o")
	git("add", ".")
	git("commit", "-q", "-m", "first")
	write("a.txt", "changed")
	write("new.txt", "n")
	write("loose file.txt", "l")
	git("add", "new.txt")
	git("mv", "old name.txt", "new name.txt")

	call := func(fn, dir string) string {
		t.Helper()
		resp, err := c.Try("git.puff", string(src), fn, "[]", dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("try git %s: %v %s", fn, err, resp.Error)
		}
		return resp.Value
	}
	// trace is where branch's try passes git's error up, as file:line:col.
	before, _, _ := strings.Cut(string(src), `(try (git ["branch"`)
	trace := fmt.Sprintf("git.puff:%d:%d", strings.Count(before, "\n")+1, len(before)-strings.LastIndex(before, "\n"))
	// git names the repository by its real path: /private/var on macOS.
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ fn, dir, want string }{
		{"branch", repo, `(ok "main")`},
		{"status", repo, `(ok {:ahead 0 :behind 0 :branch (some "main") :files [` +
			`{:index :none :path "a.txt" :work :modified} ` +
			`{:index :renamed :path "new name.txt" :work :none} ` +
			`{:index :added :path "new.txt" :work :none} ` +
			`{:index :untracked :path "loose file.txt" :work :untracked}] :upstream (none)})`},
		{"changes", repo, `(ok (locations [` +
			`{:file "` + root + `/a.txt" :note "modified"} ` +
			`{:file "` + root + `/new name.txt" :note "renamed"} ` +
			`{:file "` + root + `/new.txt" :note "added"} ` +
			`{:file "` + root + `/loose file.txt" :note "untracked"}]))`},
		{"branch", dir, `(err {:exit 128 :kind :exit :message "git exited 128: fatal: not a git repository (or any of the parent directories): .git" :stderr "fatal: not a git repository (or any of the parent directories): .git\n" :stdout "" :trace ["` + trace + `"]})`},
	}
	for _, tt := range tests {
		if got := call(tt.fn, tt.dir); got != tt.want {
			t.Errorf("git/%s in %s =\n%s\nwant\n%s", tt.fn, tt.dir, got, tt.want)
		}
	}

	git("checkout", "-q", "--detach")
	if got := call("branch", repo); !strings.HasPrefix(got, "(err {:kind :detached") {
		t.Errorf("git/branch when detached = %s", got)
	}
	if got := call("status", repo); !strings.Contains(got, ":branch (none)") {
		t.Errorf("git/status when detached = %s", got)
	}
}

// TestGhService tries services/gh.puff with a fake gh on PATH that prints
// what the real one does.
func TestGhService(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "gh.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	fake := `#!/bin/sh
case "$*" in
"pr list "*)
  echo '[{"author":{"login":"ann","is_bot":false},"createdAt":"2026-10-27T17:35:19Z","headRefName":"ann/fix","isDraft":false,"number":12,"reviewDecision":"APPROVED","title":"Fix it","url":"https://github.com/o/r/pull/12"},{"author":{"login":"bob"},"createdAt":"2026-10-26T08:00:00Z","headRefName":"wip","isDraft":true,"number":11,"reviewDecision":"","title":"WIP","url":"https://github.com/o/r/pull/11"}]' ;;
"pr checks --json "*)
  echo '[{"bucket":"pass","link":"https://l/1","name":"build","workflow":"CI"},{"bucket":"pending","link":"https://l/2","name":"test","workflow":"CI"}]'
  exit 8 ;;
"pr checks 12 "*)
  echo '[{"bucket":"fail","link":"https://l/3","name":"lint","workflow":"CI"}]'
  exit 1 ;;
"pr checks 7 "*)
  echo "no checks reported on the 'ann/fix' branch" >&2
  exit 1 ;;
*)
  echo 'no pull requests found for branch "main"' >&2
  exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start(t, dir)
	c := dial(t, dir)

	tests := []struct{ fn, args, want string }{
		{"prs", "[]", `(ok [` +
			`{:author "ann" :branch "ann/fix" :created 2026-10-27T17:35:19Z :draft #f :number 12 :review :approved :title "Fix it" :url "https://github.com/o/r/pull/12"} ` +
			`{:author "bob" :branch "wip" :created 2026-10-26T08:00:00Z :draft #t :number 11 :review :none :title "WIP" :url "https://github.com/o/r/pull/11"}])`},
		{"checks", "[]", `(ok [{:bucket :pass :link "https://l/1" :name "build" :workflow "CI"} {:bucket :pending :link "https://l/2" :name "test" :workflow "CI"}])`},
		{"checks-of", "[12]", `(ok [{:bucket :fail :link "https://l/3" :name "lint" :workflow "CI"}])`},
		{"checks-of", "[7]", `(ok [])`},
		{"checks-of", "[3]", `(err {:exit 1 :kind :gh :message "no pull requests found for branch \"main\""})`},
		{"checks-of", `["x"]`, `(err {:kind :bad-call :message "want a pull request's number, got \"x\""})`},
	}
	for _, tt := range tests {
		resp, err := c.Try("gh.puff", string(src), tt.fn, tt.args, dir)
		if err != nil || resp.Error != "" || resp.Value != tt.want {
			t.Errorf("gh/%s %s = %s %s %v\nwant %s", tt.fn, tt.args, resp.Value, resp.Error, err, tt.want)
		}
	}
}

// TestGhWatch runs services/gh.puff, polling every 20ms, on a repository
// whose fake gh prints its prs.json.
func TestGhWatch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "gh.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	fake := `#!/bin/sh
case "$*" in
"pr list "*) cat prs.json ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	prs := func(json string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "prs.json"), []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pr12 := `{"author":{"login":"ann"},"createdAt":"2026-10-27T17:35:19Z","headRefName":"ann/fix","isDraft":false,"number":12,"reviewDecision":"APPROVED","title":"Fix it","url":"u12"}`
	pr13 := `{"author":{"login":"bob"},"createdAt":"2026-10-28T08:00:00Z","headRefName":"bob/new","isDraft":true,"number":13,"reviewDecision":"","title":"New","url":"u13"}`
	prs("[" + pr12 + "]")
	writeServices(t, dir, map[string]string{
		"gh": strings.Replace(string(src), "(def every 5m)", "(def every 20ms)", 1),
		"seen": `(srv seen :state []
  (on :gh/changed (s v) :update (ok (vector/append s v)))
  (on (numbers s) "The numbers of each change's pull requests."
    (ok (vector/map s (fn (v) (match v [{:prs ps} (vector/map ps (fn (p) (match p [{:number n} n])))]))))))`,
	})
	start(t, dir)
	c := dial(t, dir)

	eval := func(src, wd, want string) {
		t.Helper()
		if resp, err := c.Eval(src, wd); err != nil || resp.Value != want {
			t.Errorf("%s in %s = %s %s %v, want %s", src, wd, resp.Value, resp.Error, err, want)
		}
	}
	numbers := func() string {
		resp, _ := c.Eval(`(call :seen/numbers)`, "")
		return resp.Value
	}
	eval(`(call :gh/watch)`, repo, `(ok :watching)`)
	eval(`(call :gh/watching)`, "", `(ok [`+puff.Print(puff.String(repo))+`])`)
	// Watching takes what is there now; only a change is published.
	time.Sleep(100 * time.Millisecond)
	if got := numbers(); got != `(ok [])` {
		t.Errorf("seen before a change: %s", got)
	}
	prs("[" + pr13 + "," + pr12 + "]")
	eventually(t, "the change", numbers, `(ok [[13 12]])`)
	eval(`(call :gh/poll)`, "", `(ok [])`)

	// A directory gh fails in isn't watched.
	eval(`(match (call :gh/watch) [(err {:kind k}) k])`, dir, `:gh`)
	eval(`(call :gh/unwatch)`, repo, `(ok :done)`)
	eval(`(call :gh/watching)`, "", `(ok [])`)
	prs("[]")
	time.Sleep(100 * time.Millisecond)
	if got := numbers(); got != `(ok [[13 12]])` {
		t.Errorf("seen after unwatch: %s", got)
	}
}

// TestTodoService tries services/todo.puff on a TODO.md in a scratch
// directory.
func TestTodoService(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "todo.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0o700)
	todo := filepath.Join(work, "TODO.md")
	if err := os.WriteFile(todo, []byte(`# TODO

- [ ] under the title

## v1

- [x] done
- [ ] a long one,
      on two lines
  - [ ] nested
* [X] star
- not an item
  not a continuation

## v2

- [ ] last`), 0o600); err != nil {
		t.Fatal(err)
	}

	call := func(fn, args, dir string) string {
		t.Helper()
		resp, err := c.Try("todo.puff", string(src), fn, args, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("try todo %s %s: %v %s", fn, args, err, resp.Error)
		}
		return resp.Value
	}
	tests := []struct{ fn, args, want string }{
		{"items", "[]", `(ok [{:done #f :line 3 :section "TODO" :text "under the title"} ` +
			`{:done #t :line 7 :section "v1" :text "done"} ` +
			`{:done #f :line 8 :section "v1" :text "a long one, on two lines"} ` +
			`{:done #f :line 10 :section "v1" :text "nested"} ` +
			`{:done #t :line 11 :section "v1" :text "star"} ` +
			`{:done #f :line 17 :section "v2" :text "last"}])`},
		{"add", `["  new one "]`, `(ok 18)`},
		{"done", "[8]", `(ok :done)`},
		{"done", "[8]", `(err {:kind :no-item :message "no open item at line 8"})`},
		{"done", "[12]", `(err {:kind :no-item :message "no open item at line 12"})`},
		{"open", "[]", `(ok [{:done #f :line 3 :section "TODO" :text "under the title"} ` +
			`{:done #f :line 10 :section "v1" :text "nested"} ` +
			`{:done #f :line 17 :section "v2" :text "last"} ` +
			`{:done #f :line 18 :section "v2" :text "new one"}])`},
		{"view", "[]", `(ok (items [` +
			`{:action [:todo/done 3] :detail "TODO.md:3  TODO" :label "under the title"} ` +
			`{:action [:todo/done 10] :detail "TODO.md:10  v1" :label "nested"} ` +
			`{:action [:todo/done 17] :detail "TODO.md:17  v2" :label "last"} ` +
			`{:action [:todo/done 18] :detail "TODO.md:18  v2" :label "new one"}]))`},
		{"add", `["first"]`, `(ok 1)`}, // in dir, which has no TODO.md
	}
	for i, tt := range tests {
		in := work
		if i == len(tests)-1 {
			in = dir
		}
		if got := call(tt.fn, tt.args, in); got != tt.want {
			t.Errorf("todo/%s %s =\n%s\nwant\n%s", tt.fn, tt.args, got, tt.want)
		}
	}
	b, _ := os.ReadFile(todo)
	if want := "- [x] a long one,\n      on two lines\n"; !strings.Contains(string(b), want) || !strings.HasSuffix(string(b), "- [ ] last\n- [ ] new one\n") {
		t.Errorf("TODO.md after add and done:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "TODO.md")); string(b) != "- [ ] first\n" {
		t.Errorf("a new TODO.md = %q", b)
	}
}

// TestReviewerService tries services/reviewer.puff with a fake claude on
// PATH, which says what it was given.
func TestReviewerService(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "reviewer.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	// It fails unless it has no tools, and answers with the diff's added lines.
	fake := `#!/bin/sh
[ "$1 $2 $3" = "-p --tools " ] || { echo "want no tools, got $*" >&2; exit 9; }
echo "reviewed:"; grep '^+[^+]'
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start(t, dir)
	c := dial(t, dir)

	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	try := func() string {
		t.Helper()
		resp, err := c.Try("reviewer.puff", string(src), "review", "[]", repo)
		if err != nil || resp.Error != "" {
			t.Fatalf("try reviewer review: %v %s", err, resp.Error)
		}
		return resp.Value
	}
	if got := try(); !strings.HasPrefix(got, `(err {:kind :nothing :message "no uncommitted changes to review"`) {
		t.Errorf("review with no changes = %s", got)
	}

	// A tracked file, changed: the review is the trial caller's to wait for.
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o600)
	cmd := exec.Command("git", "add", "a.go")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	got := try()
	if !strings.HasPrefix(got, "(err {:id ") {
		t.Fatalf("review = %s, want a pending", got)
	}
	id := strings.Fields(strings.TrimPrefix(got, "(err {:id "))[0]
	resp, err := c.Message("wait", "", "["+id+"]", "")
	if want := `(ok "reviewed:\n+package a\n")`; err != nil || resp.Value != want {
		t.Errorf("wait %s = %s %v, want %s (try said %s)", id, resp.Value, err, want, got)
	}
}

// TestAssistantService runs services/assistant.puff, installed, with a
// fake claude on PATH that says whether it starts or resumes the session
// and echoes the message.
func TestAssistantService(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "assistant.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	fake := `#!/bin/sh
[ "$1 $2 $3" = "-p --tools " ] || { echo "want no tools, got $*" >&2; exit 9; }
case "$*" in *--session-id*) mode=new ;; *--resume*) mode=resume ;; esac
for a; do case "$prev" in --session-id|--resume) echo "$a" > last-id ;; esac; prev=$a; done
msg=$(cat)
case "$msg" in slow) sleep 1 ;; fail) echo "it broke" >&2; exit 1 ;; esac
echo "$mode in $(basename "$PWD"): $msg"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	// helper is a copy under another name whose home began a conversation
	// before agents kept a session file: it carries on with that one.
	helperHome := filepath.Join(dir, ".whiff", "agents", "helper")
	os.MkdirAll(helperHome, 0o700)
	os.WriteFile(filepath.Join(helperHome, "started"), []byte("old-id"), 0o600)
	writeServices(t, dir, map[string]string{
		"assistant": string(src),
		"helper":    strings.Replace(string(src), "(srv assistant", "(srv helper", 1),
	})
	start(t, dir)
	c := dial(t, dir)

	tell := func(text string) string {
		t.Helper()
		resp, err := c.Eval(`(call :assistant/tell "`+text+`")`, "")
		if err != nil || resp.Error != "" {
			t.Fatalf("tell %s: %v %s", text, err, resp.Error)
		}
		return resp.Value
	}
	// inboxed waits for a line in :me's inbox holding want and returns its id.
	inboxed := func(want string) string {
		t.Helper()
		var id string
		eventually(t, want, func() string {
			text, _ := c.Inbox("me")
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, want) {
					id = strings.Fields(line)[0]
					return want
				}
			}
			return text
		}, want)
		return id
	}

	if got := tell("hello"); got != "(ok :thinking)" {
		t.Fatalf("tell = %s", got)
	}
	first := inboxed(`:assistant -> :me/tell "new in assistant: hello"`)

	// Your reply to its answer is the next turn, in the same session.
	if _, err := c.Message("reply", "", "["+first+` (ok "and more")]`, ""); err != nil {
		t.Fatal(err)
	}
	second := inboxed(`:assistant -> :me/tell "resume in assistant: and more"`)

	// One turn at a time.
	tell("slow")
	if got := tell("too soon"); !strings.HasPrefix(got, "(err {:kind :busy") {
		t.Errorf("tell while busy = %s", got)
	}
	inboxed(`"resume in assistant: slow"`)

	// Dismissing an answer starts nothing; a failed turn says so.
	if _, err := c.Message("reply", "", "["+second+" (ok :seen)]", ""); err != nil {
		t.Fatal(err)
	}
	tell("fail")
	inboxed(`:assistant -> :me/tell "I couldn't answer: {:exit 1 :kind :exit :message \"job `)
	inboxed(` exited 1\" :stderr \"it broke\\n\"}"`)
	// The first was replied to and the second dismissed: slow's and fail's wait.
	if text, _ := c.Inbox("me"); strings.Count(text, "\n") != 2 {
		t.Errorf("want two answers waiting:\n%s", text)
	}
	// Its turns are the agent's, whoever wrote to it.
	historyHas(t, c, `  call  agent  :assistant -> :me/tell "new in assistant: hello"`)

	// forget starts a new session, with a new id.
	home := filepath.Join(dir, ".whiff", "agents", "assistant")
	old, _ := os.ReadFile(filepath.Join(home, "session"))
	if resp, err := c.Eval(`(call :assistant/forget)`, ""); err != nil || resp.Value != "(ok :forgotten)" {
		t.Fatalf("forget = %+v %v", resp, err)
	}
	tell("again")
	inboxed(`"new in assistant: again"`)
	if now, _ := os.ReadFile(filepath.Join(home, "session")); len(now) == 0 || string(now) == string(old) {
		t.Errorf("session after forget = %q, before %q: want a new one", now, old)
	}

	if resp, err := c.Eval(`(call :helper/tell "hi")`, ""); err != nil || resp.Value != "(ok :thinking)" {
		t.Fatalf("helper/tell = %+v %v", resp, err)
	}
	inboxed(`:helper -> :me/tell "resume in helper: hi"`)
	if id, _ := os.ReadFile(filepath.Join(helperHome, "last-id")); string(id) != "old-id\n" {
		t.Errorf("helper resumed %q, want old-id", id)
	}
}

// TestPipelineService runs services/pipeline.puff with a fake claude on
// PATH: the dev writes feature.txt in its first turn and test.txt when
// resumed; the reviewer approves, or, once picky, never does.
func TestPipelineService(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	fake := filepath.Join(dir, "fake")
	bin := filepath.Join(dir, "bin")
	os.Mkdir(fake, 0o700)
	os.Mkdir(bin, 0o700)
	claude := `#!/bin/sh
if [ "$1 $2 $3 $4" = "-p --strict-mcp-config --tools " ]; then
  cat > "$FAKE/review-input"
  echo "review $(basename "$PWD")" >> "$FAKE/log"
  if [ -f "$FAKE/picky" ]; then echo "needs work"; echo "VERDICT: changes"; else echo "looks good"; echo "VERDICT: approve"; fi
  exit 0
fi
cat > /dev/null
case " $* " in
  *" --resume "*) echo "dev resume $(basename "$PWD")" >> "$FAKE/log"; echo t > test.txt ;;
  *" --session-id "*) echo "dev new $(basename "$PWD")" >> "$FAKE/log"; echo f > feature.txt ;;
  *) echo "want a session, got $*" >&2; exit 9 ;;
esac
echo "changed things"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE", fake)
	t.Setenv("HOME", dir)
	writeServices(t, dir, map[string]string{"pipeline": string(src)})
	start(t, dir)
	c := dial(t, dir)
	if _, err := c.Sub("pipeline/done"); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "first")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	call := func(fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", "pipeline/"+fn, args, repo)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	done := func() string {
		t.Helper()
		select {
		case e := <-c.Events():
			return e.Value
		case <-time.After(5 * time.Second):
			h, _ := historyOf(c)
			log, _ := os.ReadFile(filepath.Join(fake, "log"))
			t.Fatalf("no :pipeline/done; claude ran:\n%s\nhistory:\n%s", log, h)
			return ""
		}
	}
	// The check wants test.txt, which the dev writes only in its second
	// round: an approving review doesn't end a run whose check fails.
	check := `["sh" "-c" "test -f test.txt"]`
	if got := call("start", `["add a feature" `+check+`]`); got != "(ok 1)" {
		t.Fatalf("start = %s", got)
	}
	wt := filepath.Join(dir, ".whiff", "pipelines", "repo-1")
	if got := done(); !strings.Contains(got, ":status :ready") || !strings.Contains(got, `:branch "whiff/1"`) {
		t.Errorf("done = %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fake, "log")); string(b) != "dev new repo-1\nreview repo-1\ndev resume repo-1\nreview repo-1\n" {
		t.Errorf("claude was run:\n%s", b)
	}
	if got := git(repo, "show", "--name-only", "--format=%s", "whiff/1"); got != "add a feature\n\nfeature.txt\ntest.txt\n" {
		t.Errorf("whiff/1's commit = %q", got)
	}
	if got := git(repo, "status", "--porcelain"); got != "" {
		t.Errorf("the working tree changed: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fake, "review-input")); !strings.Contains(string(b), "Task: add a feature") ||
		!strings.Contains(string(b), "The check passed") || !strings.Contains(string(b), "+t") {
		t.Errorf("the last review was given:\n%s", b)
	}
	if text, _ := c.Inbox("me"); !strings.Contains(text, "pipeline 1, add a feature: ready. Committed on whiff/1 in "+wt+", after 2 rounds.") {
		t.Errorf("your inbox = %s", text)
	}

	// A picky reviewer: the run gives up after 3 rounds.
	os.WriteFile(filepath.Join(fake, "picky"), nil, 0o600)
	if got := call("start", `["another" `+check+`]`); got != "(ok 2)" {
		t.Fatalf("start = %s", got)
	}
	if got := done(); !strings.Contains(got, ":status :gave-up") {
		t.Errorf("done = %s", got)
	}
	if got := call("runs", "[]"); !strings.Contains(got, `{:branch "whiff/1" :dir "`+wt+`" :round 2 :run 1 :step :ready :task "add a feature"}`) ||
		!strings.Contains(got, ":round 3 :run 2 :step :gave-up") {
		t.Errorf("runs = %s", got)
	}
	if got := call("view", "[]"); !strings.Contains(got, `{:detail "gave-up, round 3, whiff/2" :label "2  another"}`) {
		t.Errorf("view = %s", got)
	}
	if got := call("cancel", "[2]"); !strings.Contains(got, ":no-run") {
		t.Errorf("cancel of an ended run = %s", got)
	}

	// Outside a repository nothing starts.
	resp, err := c.Message("call", "pipeline/start", `["x" `+check+`]`, dir)
	if err != nil || !strings.Contains(resp.Value, "not a git repository") {
		t.Errorf("start outside a repository = %+v %v", resp, err)
	}
}

// TestPlanService runs services/plan.puff over services/pipeline.puff
// with a fake claude: the planner answers two tasks in a code fence, the
// dev writes a file named after its task, and the reviewer approves.
func TestPlanService(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	srcs := map[string]string{}
	for _, name := range []string{"plan", "pipeline"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "services", name+".puff"))
		if err != nil {
			t.Fatal(err)
		}
		srcs[name] = string(src)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	claude := `#!/bin/sh
case "$4" in
  Read,Glob,Grep) cat > /dev/null; printf 'Here is the plan:\n` + "```" + `json\n["one", "two"]\n` + "```" + `\n' ;;
  "") cat > /dev/null; echo "VERDICT: approve" ;;
  *) task=$(sed -n 's/^Task: //p'); echo "$task" > "$task.txt"; echo "wrote $task.txt" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	writeServices(t, dir, srcs)
	start(t, dir)
	c := dial(t, dir)

	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")

	resp, err := c.Message("call", "plan/start", `["two files" ["true"]]`, repo)
	if err != nil || resp.Value != "(ok 1)" {
		t.Fatalf("plan/start = %+v %v", resp, err)
	}
	// Only the plan tells you: its runs are quiet.
	want := `:plan -> :me/tell "plan 1, two files: done. All 2 tasks built; whiff/2 has them all."` + "\n"
	eventually(t, "the plan", func() string {
		text, _ := c.Inbox("me")
		if _, tell, ok := strings.Cut(text, "  "); ok && strings.Count(text, "\n") == 1 {
			return tell
		}
		return text
	}, want)
	if got := git("log", "--format=%s", "whiff/2"); got != "two\none\nfirst\n" {
		t.Errorf("whiff/2's log = %q", got)
	}
	if got := git("show", "--name-only", "--format=", "whiff/2"); got != "two.txt\n" {
		t.Errorf("whiff/2's own commit = %q", got)
	}
	resp, err = c.Message("call", "plan/plans", "[]", repo)
	if err != nil || !strings.Contains(resp.Value, `:step :done :task 1 :tasks ["one" "two"]`) {
		t.Errorf("plans = %+v %v", resp, err)
	}
}
