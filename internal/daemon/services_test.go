package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
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
	before, _, _ := strings.Cut(string(src), `(try (proc/output ["git" "branch"`)
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
		"seen": `(service seen :state []
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
			`{:actions [{:action [:todo/done 3] :label "Tick it"}] :detail "TODO.md:3  TODO" :label "under the title"} ` +
			`{:actions [{:action [:todo/done 10] :label "Tick it"}] :detail "TODO.md:10  v1" :label "nested"} ` +
			`{:actions [{:action [:todo/done 17] :label "Tick it"}] :detail "TODO.md:17  v2" :label "last"} ` +
			`{:actions [{:action [:todo/done 18] :label "Tick it"}] :detail "TODO.md:18  v2" :label "new one"}]))`},
		{"board", "[]", `(ok (board [` +
			`{:items [{:actions [{:action [:todo/done 3] :label "Tick it"}] :detail "TODO.md:3" :label "under the title"}] :title "TODO"} ` +
			`{:items [{:actions [{:action [:todo/done 10] :label "Tick it"}] :detail "TODO.md:10" :label "nested"}] :title "v1"} ` +
			`{:items [{:actions [{:action [:todo/done 17] :label "Tick it"}] :detail "TODO.md:17" :label "last"} ` +
			`{:actions [{:action [:todo/done 18] :label "Tick it"}] :detail "TODO.md:18" :label "new one"}] :title "v2"}]))`},
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

// fakeClaude puts a fake claude on PATH, with HOME at dir. It answers in
// --output-format json, at the cost in a file cost in its directory, or
// $0.01. It says whether it starts or resumes the session and echoes the
// message; "slow" takes a second, "fail" exits 1 saying so on stderr,
// "spend" reaches its --max-budget-usd, and "hold" waits for a file go in
// its directory.
// A turn with whiff mcp's flags leaves its WHIFF_SESSION in a file token,
// and its arguments, a line each, in args. A scheduled turn of an agent whose prompt is
// "Watch the thread." has nothing to say the first time, then news from
// its model, then nothing again.
func fakeClaude(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	fake := `#!/bin/sh
[ "$1 $2 $3" = "-p --tools " ] || { echo "want no tools, got $*" >&2; exit 9; }
case "$*" in *--session-id*) mode=new ;; *--resume*) mode=resume ;; esac
model=default
for a; do case "$prev" in --session-id|--resume) echo "$a" > last-id ;; --model) model=$a ;; --append-system-prompt) sys=$a ;; --max-budget-usd) budget=$a ;; esac; prev=$a; done
case "$*" in *"--output-format json"*) ;; *) echo "want json, got $*" >&2; exit 9 ;; esac
cost=$(cat cost 2>/dev/null || echo 0.01)
answer() { printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","total_cost_usd":%s}\n' "$1" "$cost"; }
msg=$(cat)
case "$*" in *--mcp-config*)
  case "$*" in *"--strict-mcp-config --allowedTools mcp__whiff"*) ;; *) echo "want whiff mcp alone, got $*" >&2; exit 9 ;; esac
  echo "$WHIFF_SESSION" > token; printf '%s\n' "$@" > args ;;
esac
case "$msg" in *"scheduled turn"*)
  case "$sys" in *"Watch the thread."*) ;; *) echo "no prompt in $sys" >&2; exit 9 ;; esac
  if [ -e told ]; then answer nothing; elif [ -e woke ]; then touch told; answer "news from $model"; else touch woke; answer " Nothing "; fi
  exit ;;
esac
case "$msg" in spend)
  printf '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"errors":["Reached maximum budget ($%s)"],"total_cost_usd":%s}\n' "$budget" "$cost"
  exit 1 ;;
esac
case "$msg" in slow) sleep 1 ;; fail) echo "it broke" >&2; exit 1 ;; hold) while [ ! -e go ]; do sleep 0.02; done ;; esac
answer "$mode in $(basename "$PWD"): $msg"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
}

// TestAssistantService runs services/assistant.puff, installed, with a
// fake claude on PATH that says whether it starts or resumes the session
// and echoes the message; and watcher, an agent with :model and :every,
// whose first scheduled turn has nothing to say and its second news.
func TestAssistantService(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "assistant.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	fakeClaude(t, dir)
	// helper is a copy under another name whose home began a conversation
	// before agents kept a session file: it carries on with that one.
	helperHome := filepath.Join(dir, ".whiff", "agents", "helper")
	os.MkdirAll(helperHome, 0o700)
	os.WriteFile(filepath.Join(helperHome, "started"), []byte("old-id"), 0o600)
	writeServices(t, dir, map[string]string{
		"assistant": string(src),
		"helper":    strings.Replace(string(src), "(agent assistant", "(agent helper", 1),
		"watcher":   `(agent watcher :prompt "Watch the thread." :model "opus" :every 1s)`,
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
	if text, _ := c.Inbox("me"); strings.Count(text, ":assistant -> ") != 2 {
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

	// A scheduled turn's "nothing" stays out of your inbox; its news doesn't.
	inboxed(`:watcher -> :me/tell "news from opus"`)
	if text, _ := c.Inbox("me"); strings.Contains(strings.ToLower(text), "nothing") {
		t.Errorf("a scheduled turn's nothing reached the inbox:\n%s", text)
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
  if [ -f "$FAKE/broken" ]; then echo "stuck" >&2; exit 1; fi
  if [ -f "$FAKE/picky" ]; then echo "needs work"; echo "VERDICT: changes"; else echo "looks good"; echo "VERDICT: approve"; fi
  exit 0
fi
cat > "$FAKE/dev-input"
case " $* " in
  *" --resume "*) echo "dev resume $(basename "$PWD")" >> "$FAKE/log"; echo t > test.txt ;;
  *" --session-id "*) echo "dev new $(basename "$PWD")" >> "$FAKE/log"; basename "$PWD" > feature.txt ;;
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
	if got := call("runs", "[]"); !strings.Contains(got, `{:branch "whiff/1" :dir "`+wt+`" :from :review :round 2 :run 1 :step :ready :task "add a feature"}`) ||
		!strings.Contains(got, ":round 3 :run 2 :step :gave-up") {
		t.Errorf("runs = %s", got)
	}
	retry := `{:action [:pipeline/retry 2] :ask "A note for the developer, if any" :label "Retry with a note"} {:action [:pipeline/discard 2] :label "Discard"}`
	if got := call("view", "[]"); !strings.Contains(got, `{:actions [{:action [:pipeline/show 2] :label "Show review and diff"} `+retry+`] :detail "gave-up, round 3, whiff/2" :label "2  another"}`) {
		t.Errorf("view = %s", got)
	}
	if got := call("cancel", "[2]"); !strings.Contains(got, ":no-run") {
		t.Errorf("cancel of an ended run = %s", got)
	}

	// Retried with a note, it goes on in the dev's session, and the
	// reviewer, no longer picky, approves.
	os.Remove(filepath.Join(fake, "picky"))
	if got := call("retry", `[2 "keep it small"]`); got != "(ok :retrying)" {
		t.Fatalf("retry = %s", got)
	}
	if got := done(); !strings.Contains(got, ":status :ready") {
		t.Errorf("done after a retry = %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fake, "dev-input")); !strings.HasPrefix(string(b), "A note from the user:\n\nkeep it small\n\nThe last review:\n\nneeds work") {
		t.Errorf("the retried dev was told:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(fake, "log")); !strings.HasSuffix(string(b), "dev resume repo-2\nreview repo-2\n") {
		t.Errorf("claude was run:\n%s", b)
	}
	if got := call("runs", "[]"); !strings.Contains(got, ":round 4 :run 2 :step :ready") {
		t.Errorf("runs after a retry = %s", got)
	}
	if got := call("retry", `[2 ""]`); !strings.Contains(got, ":no-run") {
		t.Errorf("retry of a ready run = %s", got)
	}

	// Merging fast-forwards your branch to a ready run's; once it has
	// moved on, another run's branch doesn't merge.
	if got := call("merge", "[2]"); got != `(ok "whiff/2")` {
		t.Errorf("merge = %s", got)
	}
	if got := git(repo, "log", "--format=%s", "main"); got != "another\nfirst\n" {
		t.Errorf("main's log after the merge = %q", got)
	}
	if got := call("merge", "[1]"); !strings.Contains(got, ":kind :exit") {
		t.Errorf("merge of a run main moved past = %s", got)
	}
	// A merged run's worktree and branch are gone; a discarded one's too.
	gone := func(n string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, ".whiff", "pipelines", "repo-"+n)); !os.IsNotExist(err) {
			t.Errorf("run %s's worktree is still there: %v", n, err)
		}
		if got := git(repo, "branch", "--list", "whiff/"+n); got != "" {
			t.Errorf("run %s's branch is still there: %q", n, got)
		}
	}
	gone("2")
	if got := call("discard", "[1]"); got != "(ok :discarded)" {
		t.Errorf("discard = %s", got)
	}
	gone("1")
	if got := call("runs", "[]"); !strings.Contains(got, ":run 1 :step :discarded") || !strings.Contains(got, ":run 2 :step :merged") {
		t.Errorf("runs after merge and discard = %s", got)
	}
	if got := call("discard", "[2]"); !strings.Contains(got, ":no-run") {
		t.Errorf("discard of a merged run = %s", got)
	}

	// A reviewer that fails fails the run, which offers Review again: it
	// reruns only the reviewer, in the same round, and the run goes on. Its
	// check passes at once, as main has test.txt now.
	os.WriteFile(filepath.Join(fake, "broken"), nil, 0o600)
	if got := call("start", `["a third" `+check+`]`); got != "(ok 3)" {
		t.Fatalf("start = %s", got)
	}
	if got := done(); !strings.Contains(got, ":status :failed") || !regexp.MustCompile(`The reviewer failed in round 1 \(task (\d+), whiff wait \d+\): job \d+ exited 1:\\n\\nstuck`).MatchString(got) {
		t.Errorf("done = %s", got)
	}
	again := `{:action [:pipeline/review-again 3] :label "Review again"}`
	if text, _ := c.Inbox("me"); !strings.Contains(text, again) {
		t.Errorf("your inbox = %s", text)
	}
	if got := call("view", "[]"); !strings.Contains(got, `{:action [:pipeline/show 3] :label "Show review and diff"} `+again) {
		t.Errorf("view = %s", got)
	}
	os.Remove(filepath.Join(fake, "broken"))
	if got := call("review-again", "[3]"); got != "(ok :reviewing)" {
		t.Fatalf("review-again = %s", got)
	}
	if got := done(); !strings.Contains(got, ":status :ready") {
		t.Errorf("done after reviewing again = %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fake, "log")); !strings.HasSuffix(string(b), "dev new repo-3\nreview repo-3\nreview repo-3\n") {
		t.Errorf("claude was run:\n%s", b)
	}
	if got := call("review-again", "[3]"); !strings.Contains(got, ":no-run") {
		t.Errorf("review-again of a ready run = %s", got)
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
  Read,Glob,Grep) cat > "$HOME/planned"; if [ -f "$HOME/tasks" ]; then cat "$HOME/tasks"; else printf 'Here is the plan:\n` + "```" + `json\n["one", "two"]\n` + "```" + `\n'; fi ;;
  "") cat > /dev/null; if [ -f "$HOME/broken" ]; then echo "stuck" >&2; exit 1; fi
     if [ -f "$HOME/picky" ]; then echo "VERDICT: changes"; else echo "VERDICT: approve"; fi ;;
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

	watch := dial(t, dir)
	if _, err := watch.Sub("plan/changed"); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Message("call", "plan/start", `["two files" ["true"]]`, repo)
	if err != nil || resp.Value != "(ok 1)" {
		t.Fatalf("plan/start = %+v %v", resp, err)
	}
	// Only the plan tells you: its runs are quiet.
	// Its report offers the last run's review and diff, and its merge.
	actions := `[{:action [:pipeline/show 2] :label "Show review and diff"} {:action [:plan/merge 1] :label "Merge whiff/2"}]`
	want := `:plan -> :me/tell "plan 1, two files: done. All 2 tasks built; whiff/2 has them all." {:actions ` + actions + `}` + "\n"
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
	resp, err = c.Message("call", "plan/board", "[]", repo)
	done := `{:items [{:actions [{:action [:pipeline/show 1] :label "Show review and diff"}] :detail "plan 1, task 1 of 2, round 1, whiff/1" :label "one"} ` +
		`{:actions ` + actions + ` :detail "plan 1, task 2 of 2, round 1, whiff/2" :label "two"}] :title "Done"}`
	if err != nil || !strings.Contains(resp.Value, `{:items [] :title "Planned"}`) || !strings.Contains(resp.Value, done) {
		t.Errorf("board = %+v %v", resp, err)
	}
	resp, err = c.Message("call", "pipeline/show", "[2]", repo)
	if err != nil || !strings.Contains(resp.Value, `VERDICT: approve`) || !strings.Contains(resp.Value, `+++ b/two.txt`) {
		t.Errorf("show = %+v %v", resp, err)
	}
	// Tickets move as the plan starts and gets its tasks, at each run's
	// dev, check and review, and as each run ends: 10 times.
	var changes []string
	for len(changes) < 10 {
		select {
		case e := <-watch.Events():
			changes = append(changes, e.Topic+" "+e.Value)
		case <-time.After(2 * time.Second):
			t.Fatalf("%d :plan/changed, want 10: %q", len(changes), changes)
		}
	}
	for _, ch := range changes {
		if ch != "plan/changed {:plan 1}" {
			t.Errorf("change = %q", ch)
		}
	}

	// A picky reviewer stops a plan at its first task; retried, it goes on.
	call := func(fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", "plan/"+fn, args, repo)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	inboxHas := func(what, s string) {
		t.Helper()
		eventually(t, what, func() string {
			if text, _ := c.Inbox("me"); !strings.Contains(text, s) {
				return text
			}
			return "there"
		}, "there")
	}
	os.WriteFile(filepath.Join(dir, "picky"), nil, 0o600)
	if got := call("start", `["more files" ["true"]]`); got != "(ok 2)" {
		t.Fatalf("plan/start = %s", got)
	}
	retry := `{:action [:plan/retry 2] :ask "A note for the developer, if any" :label "Retry with a note"} {:action [:plan/discard 2] :label "Discard"}`
	inboxHas("the plan stopped", `"plan 2, more files: failed. Task 1 of 2, run 3, ended gave-up: `)
	inboxHas("its actions", `[{:action [:pipeline/show 3] :label "Show review and diff"} `+retry+`]`)
	if got := call("board", "[]"); !strings.Contains(got, `:actions [{:action [:pipeline/show 3] :label "Show review and diff"} `+retry+`] :detail "plan 2, task 1 of 2, round 3, whiff/3"`) {
		t.Errorf("board of a stopped plan = %s", got)
	}
	os.Remove(filepath.Join(dir, "picky"))
	if got := call("retry", `[2 "go on"]`); got != "(ok :retrying)" {
		t.Fatalf("plan/retry = %s", got)
	}
	inboxHas("the retried plan", `"plan 2, more files: done. All 2 tasks built; whiff/4 has them all."`)
	if got := call("retry", `[2 ""]`); !strings.Contains(got, ":no-plan") {
		t.Errorf("retry of a done plan = %s", got)
	}

	// Merging a plan fast-forwards your branch to its last one.
	if got := call("merge", "[1]"); got != `(ok "whiff/2")` {
		t.Errorf("plan/merge = %s", got)
	}
	if got := git("log", "--format=%s", "main"); got != "two\none\nfirst\n" {
		t.Errorf("main's log after the merge = %q", got)
	}
	if got := call("merge", "[2]"); !strings.Contains(got, ":kind :exit") {
		t.Errorf("merge of a plan main moved past = %s", got)
	}
	if got := call("merge", "[3]"); !strings.Contains(got, ":no-plan") {
		t.Errorf("merge of no plan = %s", got)
	}

	// Merged, plan 1's runs are tidied away; discarded, plan 2's are too,
	// and both leave the board.
	if got := call("discard", "[2]"); got != "(ok :discarded)" {
		t.Errorf("plan/discard = %s", got)
	}
	if got := git("branch", "--list", "whiff/*"); got != "" {
		t.Errorf("branches left: %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, ".whiff", "pipelines")); len(entries) != 0 {
		t.Errorf("worktrees left: %v", entries)
	}
	if got := call("board", "[]"); strings.Contains(got, ":label") {
		t.Errorf("board after merge and discard = %s", got)
	}
	if got := call("discard", "[2]"); !strings.Contains(got, ":no-plan") {
		t.Errorf("discard of a discarded plan = %s", got)
	}

	// A draft waits with its tasks; edited and approved, it builds them.
	// (main has plan 1's one.txt and two.txt by now: new tasks change it.)
	if got := call("draft", `["drafted" ["true"]]`); got != "(ok 3)" {
		t.Fatalf("plan/draft = %s", got)
	}
	inboxHas("the draft", `"plan 3, drafted: proposed. Its tasks, in repo, on main:\n\n1. one\n2. two" {:actions [{:action [:plan/approve 3] :label "Build it"} {:action [:plan/discard 3] :label "Discard"}]}`)
	if b, _ := os.ReadFile(filepath.Join(dir, "planned")); !strings.Contains(string(b), "The check: true\n\nGoal: drafted") {
		t.Errorf("the planner read:\n%s", b)
	}
	if got := call("approve", "[1]"); !strings.Contains(got, ":no-plan") {
		t.Errorf("approve of a merged plan = %s", got)
	}
	for _, args := range []string{`[3 3 "x"]`, `[3 0 "x"]`, `[3 1 ""]`} {
		if got := call("edit", args); !strings.Contains(got, ":no-task") && !strings.Contains(got, ":bad-task") {
			t.Errorf("edit %s = %s", args, got)
		}
	}
	call("edit", `[3 1 "uno"]`)
	if got := call("edit", `[3 2 "three"]`); got != `(ok ["uno" "three"])` {
		t.Errorf("edit = %s", got)
	}
	if got := call("approve", "[3]"); got != "(ok :building)" {
		t.Fatalf("approve = %s", got)
	}
	inboxHas("the approved draft", `"plan 3, drafted: done. All 2 tasks built; whiff/6 has them all."`)
	if got := git("log", "--format=%s", "whiff/6"); got != "three\nuno\ntwo\none\nfirst\n" {
		t.Errorf("whiff/6's log = %q", got)
	}
	if got := call("edit", `[3 1 "x"]`); !strings.Contains(got, ":no-plan") {
		t.Errorf("edit of a built plan = %s", got)
	}

	// A reviewer that fails stops the plan, which offers Review again: only
	// the reviewer runs again, and the plan goes on.
	os.WriteFile(filepath.Join(dir, "tasks"), []byte(`["four"]`), 0o600)
	os.WriteFile(filepath.Join(dir, "broken"), nil, 0o600)
	if got := call("start", `["reviewed" ["true"]]`); got != "(ok 4)" {
		t.Fatalf("plan/start = %s", got)
	}
	again := `{:action [:plan/review-again 4] :label "Review again"}`
	inboxHas("the failed review", `"plan 4, reviewed: failed. Task 1 of 1, run 7, ended failed: The reviewer failed in round 1 (task `)
	inboxHas("Review again", `[{:action [:pipeline/show 7] :label "Show review and diff"} `+again+` {:action [:plan/retry 4]`)
	if got := call("board", "[]"); !strings.Contains(got, `{:action [:pipeline/show 7] :label "Show review and diff"} `+again) {
		t.Errorf("board of a plan whose reviewer failed = %s", got)
	}
	os.Remove(filepath.Join(dir, "broken"))
	if got := call("review-again", "[4]"); got != "(ok :reviewing)" {
		t.Fatalf("plan/review-again = %s", got)
	}
	inboxHas("the plan reviewed again", `"plan 4, reviewed: done. All 1 tasks built; whiff/7 has them all."`)
	if got := call("review-again", "[4]"); !strings.Contains(got, ":no-plan") {
		t.Errorf("review-again of a done plan = %s", got)
	}
}

// TestServiceFileTests runs the (test ...)s in every file in services/, as
// whiff test does.
func TestServiceFileTests(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	files, err := filepath.Glob(filepath.Join("..", "..", "services", "*.puff"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no services: %v", err)
	}
	// Examples' services, in a directory each, have tests too; plain puff
	// files beside them are cmd/puff's TestExamples'.
	examples, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*", "*.puff"))
	for _, file := range examples {
		if src, err := os.ReadFile(file); err == nil && strings.Contains(string(src), "(service ") {
			files = append(files, file)
		}
	}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// From where the file is, so its uses are found, as whiff test
		// from the repository would.
		wd, _ := os.Getwd()
		resp, err := c.Test(file, string(src), wd)
		if err != nil || resp.Error != "" {
			t.Errorf("%s: %+v %v", file, resp, err)
			continue
		}
		v, err := puff.ReadValue(resp.Value)
		results, ok := v.(puff.Vector)
		if err != nil || !ok {
			t.Fatalf("%s: results %s", file, resp.Value)
		}
		for i := range results.Len() {
			r := results.At(i).(puff.Map)
			if msg, _ := r.Get(puff.Keyword("message")); msg != puff.String("") {
				at, _ := r.Get(puff.Keyword("at"))
				name, _ := r.Get(puff.Keyword("test"))
				t.Errorf("%s %s: %s", at, name, msg)
			}
		}
	}
}

// TestPipelineRecovers stops whiff while services/pipeline.puff's dev runs:
// the run fails with a report that offers Retry, and retrying finishes it.
func TestPipelineRecovers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	hang := filepath.Join(dir, "hang")
	os.Mkdir(bin, 0o700)
	// The dev hangs while hang is there; the reviewer approves.
	claude := `#!/bin/sh
if [ "$1 $2 $3 $4" = "-p --strict-mcp-config --tools " ]; then
  cat > /dev/null
  echo "looks good"; echo "VERDICT: approve"
  exit 0
fi
cat > /dev/null
if [ -f "` + hang + `" ]; then exec sleep 30; fi
echo f > feature.txt
echo "changed things"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hang, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	writeServices(t, dir, map[string]string{"pipeline": string(src)})
	done := start(t, dir)
	c := dial(t, dir)

	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	call := func(fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", "pipeline/"+fn, args, repo)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	if got := call("start", `["add a feature" ["true"]]`); got != "(ok 1)" {
		t.Fatalf("start = %s", got)
	}
	eventually(t, "the dev running", func() string { return strconv.FormatBool(strings.Contains(call("runs", "[]"), ":step :dev")) }, "true")
	stopDaemon(t, c, done)

	if err := os.Remove(hang); err != nil {
		t.Fatal(err)
	}
	start(t, dir)
	c = dial(t, dir)
	eventually(t, "the run failing", func() string { return strconv.FormatBool(strings.Contains(call("runs", "[]"), ":step :failed")) }, "true")
	inbox, err := c.Message("call", "me/inbox", "[]", repo)
	if err != nil || !strings.Contains(inbox.Value, "pipeline 1, add a feature: failed.") || !strings.Contains(inbox.Value, "The developer failed in round 1 (task 1, whiff wait 1): job 1 was killed when whiff stopped") ||
		!strings.Contains(inbox.Value, "[:pipeline/retry 1]") || !strings.Contains(inbox.Value, "[:pipeline/discard 1]") {
		t.Fatalf("your inbox = %+v %v", inbox, err)
	}

	if got := call("retry", `[1 "go on"]`); !strings.HasPrefix(got, "(ok") {
		t.Fatalf("retry = %s", got)
	}
	eventually(t, "the run finishing", func() string { return strconv.FormatBool(strings.Contains(call("runs", "[]"), ":step :ready")) }, "true")
}

// TestAgentAsk asks an agent from a client and from a service: each gets
// the answer back as a whiff/done, asks wait their turn instead of failing
// :busy, a failed turn keeps its error, and a reload while a turn runs
// doesn't start it again.
func TestAgentAsk(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)
	dir := tempDir(t)
	fakeClaude(t, dir)
	sage := `(agent sage :prompt "Answer questions.")`
	writeServices(t, dir, map[string]string{
		"sage": sage,
		"boss": `(service boss
  (on (go q) "Asks sage q." (submitted (call :sage/ask q)))
  (on (whiff/done id r) "sage's answer." (call :me/tell (str "boss heard " (value/to-string r)))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	ask := func(q string) string {
		t.Helper()
		resp, err := c.Eval(`(call :sage/ask "`+q+`")`, "")
		if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
			t.Fatalf("ask %s = %+v %v, want a pending", q, resp, err)
		}
		return strings.Fields(strings.TrimPrefix(resp.Value, "(err {:id "))[0]
	}
	wait := func(id, want string) {
		t.Helper()
		resp, err := c.Message("wait", "", "["+id+"]", "")
		if err != nil || !strings.HasPrefix(resp.Value, want) {
			t.Errorf("wait %s = %s %v, want %s...", id, resp.Value, err, want)
		}
	}

	// The second ask waits in sage's inbox while the first's turn runs.
	slow, quick := ask("slow"), ask("quick")
	if text, _ := c.Inbox("sage"); strings.Count(text, ":sage/ask") != 2 {
		t.Errorf("sage's inbox:\n%s\nwant both asks", text)
	}
	// A reload hears of both again, and starts neither twice.
	writeServices(t, dir, map[string]string{"sage": sage + "\n; reloaded\n"})
	eventually(t, "slow's ask told again", func() string {
		h, _ := historyOf(c)
		return fmt.Sprint(strings.Count(h, ":sage/whiff/asked "+slow+"  ") >= 2)
	}, "true")
	wait(slow, `(ok "new in sage: slow")`)
	wait(quick, `(ok "resume in sage: quick")`)

	// A service's ask comes back to it.
	if resp, err := c.Eval(`(call :boss/go "hello")`, ""); err != nil || !strings.HasPrefix(resp.Value, "(ok ") {
		t.Fatalf("boss/go = %+v %v", resp, err)
	}
	historyHas(t, c, `:boss -> :me/tell "boss heard (ok \"resume in sage: hello\")"`)

	// A failed turn answers with its error; then sage goes on.
	wait(ask("fail"), `(err {:exit 1 :kind :exit`)
	wait(ask("after"), `(ok "resume in sage: after")`)
	turns, _ := os.ReadFile(filepath.Join(dir, ".whiff", "agents", "sage", "turns"))
	if n := strings.Count(string(turns), "\n"); n != 5 {
		t.Errorf("sage ran %d turns, want one for each of the 5 asks:\n%s", n, turns)
	}
	if text, _ := c.Inbox("sage"); text != "" {
		t.Errorf("sage's inbox after every answer:\n%s", text)
	}

	// An agent session's ask is its write: the turn goes through the
	// review gate, which refuses it here as nobody reviews, and the ask
	// is answered with why.
	agent := dial(t, dir)
	agent.Agent = true
	if _, err := agent.Claim("agent", ""); err != nil {
		t.Fatal(err)
	}
	resp, err := agent.Eval(`(call :sage/ask "from an agent")`, "")
	if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("agent's ask = %+v %v, want a pending", resp, err)
	}
	wait(strings.Fields(strings.TrimPrefix(resp.Value, "(err {:id "))[0], `(err {:kind :no-reviewer`)
}

// TestAgentSession runs an agent with :can: its turn's whiff mcp claims
// the agent's session with the token whiff gave the job, and reaches only
// the :can functions, and the reads whiff mcp lists them with, until the
// turn ends.
func TestAgentSession(t *testing.T) {
	dir := tempDir(t)
	fakeClaude(t, dir)
	os.Mkdir(filepath.Join(dir, "project"), 0o700)
	writeServices(t, dir, map[string]string{
		// Its :dir is from its file, in dir/services.
		"scout": `(agent scout :prompt "Look things up." :can [:notes/list] :dir "../project")`,
		"notes": `(service notes
  (on (list) "The notes." (ok ["a"]))
  (on (add text) "Adds a note." (file/write "note" text)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	resp, err := c.Eval(`(call :scout/ask "hold")`, "")
	if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("ask = %+v %v", resp, err)
	}
	asked := strings.Fields(strings.TrimPrefix(resp.Value, "(err {:id "))[0]
	home := filepath.Join(dir, ".whiff", "agents", "scout")
	var token string
	eventually(t, "the turn's token", func() string {
		b, _ := os.ReadFile(filepath.Join(home, "token"))
		token = strings.TrimSpace(string(b))
		return fmt.Sprint(len(token))
	}, "32")

	// Its whiff mcp acts in its :dir.
	args, _ := os.ReadFile(filepath.Join(home, "args"))
	real, _ := filepath.EvalSymlinks(filepath.Join(dir, "project"))
	if want := `"args":["mcp","--dir","` + real + `"]`; !strings.Contains(string(args), want) {
		t.Errorf("the turn's arguments:\n%s\nwant %s", args, want)
	}

	turn := dial(t, dir)
	turn.Agent = true
	claim, err := turn.ClaimSession(token, `(ask (tell text) "Leaves the agent a message.")`)
	if err != nil || claim.Value != "(ok [:agent/scout [:notes/list]])" {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	// It may call :notes/list, and describe :notes, but nothing else.
	for _, tt := range []struct{ op, target, src, want string }{
		{"call", "notes/list", "", `(ok ["a"])`},
		{"call", "notes/add", "", "error: :agent/scout may call only :notes/list, its agent's :can, not notes/add"},
		{"send", "scout/ask", "", "error: :agent/scout may call only :notes/list, its agent's :can, not scout/ask"},
		{"pub", "notes/changed", "", "error: :agent/scout may only call :notes/list, its agent's :can: no pub"},
		{"inbox", "me", "", "error: :agent/scout reads only its own inbox"},
		{"eval", "", "(session/mode)", "(ok {:mode :gated})"},
		{"eval", "", "(address/describe :notes)", "(ok [{"},
		{"eval", "", "(address/describe :scout)", "error: :agent/scout may only call :notes/list, its agent's :can: no eval"},
		{"eval", "", `(call :notes/list)`, "error: :agent/scout may only call :notes/list, its agent's :can: no eval"},
	} {
		var r rpc.Response
		var err error
		switch tt.op {
		case "eval":
			r, err = turn.Eval(tt.src, "")
		case "inbox":
			var out string
			out, err = turn.Inbox(tt.target)
			r.Value = out
			if err != nil {
				r.Error, err = err.Error(), nil
			}
		default:
			r, err = turn.Message(tt.op, tt.target, `["x"]`, "")
			if tt.target == "notes/list" {
				r, err = turn.Message(tt.op, tt.target, "[]", "")
			}
		}
		got := r.Value
		if r.Error != "" {
			got, err = "error: "+r.Error, nil
		}
		if err != nil || !strings.HasPrefix(got, tt.want) {
			t.Errorf("%s %s%s = %q %v, want %q", tt.op, tt.target, tt.src, got, err, tt.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "note")); err == nil {
		t.Error("the fenced session wrote a note")
	}

	// Once the turn ends, its token claims nothing. Its whiff mcp hangs up
	// as its claude exits.
	os.WriteFile(filepath.Join(home, "go"), nil, 0o600)
	turn.Close()
	if r, err := c.Message("wait", "", "["+asked+"]", ""); err != nil || r.Value != `(ok "new in scout: hold")` {
		t.Errorf("wait = %+v %v", r, err)
	}
	again := dial(t, dir)
	again.Agent = true
	if r, err := again.ClaimSession(token, ""); err != nil || !strings.HasPrefix(r.Value, "(err {:kind :no-session") {
		t.Errorf("claim after the turn = %+v %v", r, err)
	}

	// Its mode holds between turns: you can trust it while no turn runs,
	// and the next turn's session is trusted.
	if r, err := c.Mode("agent/scout", "trusted", []string{"notes"}, 0); err != nil || r.Value != "(ok :done)" {
		t.Fatalf("trust between turns = %+v %v", r, err)
	}
	os.Remove(filepath.Join(home, "go"))
	os.Remove(filepath.Join(home, "token"))
	resp, err = c.Eval(`(call :scout/ask "hold")`, "")
	if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("second ask = %+v %v", resp, err)
	}
	eventually(t, "the second turn's token", func() string {
		b, _ := os.ReadFile(filepath.Join(home, "token"))
		token = strings.TrimSpace(string(b))
		return fmt.Sprint(len(token))
	}, "32")
	next := dial(t, dir)
	next.Agent = true
	if r, err := next.ClaimSession(token, ""); err != nil || !strings.HasPrefix(r.Value, "(ok [:agent/scout") {
		t.Fatalf("second claim = %+v %v", r, err)
	}
	if r, err := next.Eval("(session/mode)", ""); err != nil || r.Value != "(ok {:mode :trusted :services [:notes]})" {
		t.Errorf("the second turn's mode = %+v %v", r, err)
	}
	os.WriteFile(filepath.Join(home, "go"), nil, 0o600)

	// Only a service's job gets a session.
	if r, err := c.Eval(`(call :jobs/run ["true"] {:session [:notes/list]})`, ""); err != nil || !strings.Contains(r.Value, ":session is for an agent's turn") {
		t.Errorf(":session from a client = %+v %v", r, err)
	}
}

// TestAgentLimits holds agents to their :limits: a day's dollars, which
// once spent tells you and refuses turns; a turn's, which claude stops at;
// a turn's :time; and stop, which ends the turn running now.
func TestAgentLimits(t *testing.T) {
	dir := tempDir(t)
	fakeClaude(t, dir)
	for name, cost := range map[string]string{"cheap": "0.6", "capped": "0.3"} {
		home := filepath.Join(dir, ".whiff", "agents", name)
		os.MkdirAll(home, 0o700)
		os.WriteFile(filepath.Join(home, "cost"), []byte(cost), 0o600)
	}
	writeServices(t, dir, map[string]string{
		"cheap":  `(agent cheap :prompt "p" :limits {:day 1})`,
		"capped": `(agent capped :prompt "p" :limits {:turn 0.5})`,
		"quick":  `(agent quick :prompt "p" :limits {:time 1s})`,
		"long":   `(agent long :prompt "p")`,
	})
	start(t, dir)
	c := dial(t, dir)
	ask := func(addr, q string) string {
		t.Helper()
		resp, err := c.Eval(`(call :`+addr+`/ask "`+q+`")`, "")
		if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
			t.Fatalf("%s/ask %s = %+v %v, want a pending", addr, q, resp, err)
		}
		r, err := c.Message("wait", "", "["+strings.Fields(strings.TrimPrefix(resp.Value, "(err {:id "))[0]+"]", "")
		if err != nil {
			t.Fatal(err)
		}
		return r.Value
	}
	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}

	// $0.60 a turn against $1 a day: the second turn crosses it, which
	// you hear of once, and the third doesn't start.
	if got := ask("cheap", "one"); got != `(ok "new in cheap: one")` {
		t.Errorf("one = %s", got)
	}
	if got := eval(`(call :cheap/usage)`); got != "(ok {:day 1.0 :spent 0.6 :turns 1})" {
		t.Errorf("usage = %s", got)
	}
	ask("cheap", "two")
	historyHas(t, c, `:cheap -> :me/tell "cheap has spent $1.2 in the last 24 hours, its $1.0 a day`)
	if got := ask("cheap", "three"); !strings.HasPrefix(got, `(err {:kind :over-budget :message "cheap has spent $1.2 in the last 24 hours, its $1.0 a day" :outcome :not-started`) {
		t.Errorf("three = %s", got)
	}
	if text, _ := c.Inbox("me"); strings.Count(text, "cheap has spent") != 1 {
		t.Errorf("want one notice:\n%s", text)
	}

	// A turn's own budget goes to claude, which stops at it.
	if got := ask("capped", "spend"); got != `(err {:cost 0.3 :kind :over-budget :message "the turn reached its $0.5 and stopped: Reached maximum budget ($0.5)" :outcome :stopped})` {
		t.Errorf("spend = %s", got)
	}

	// :time ends a turn that runs longer.
	if got := ask("quick", "hold"); !strings.HasPrefix(got, "(err {:kind :timeout") {
		t.Errorf("a turn past its :time = %s", got)
	}

	// stop ends the turn running now.
	resp, err := c.Eval(`(call :long/ask "hold")`, "")
	if err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("long/ask = %+v %v", resp, err)
	}
	id := strings.Fields(strings.TrimPrefix(resp.Value, "(err {:id "))[0]
	eventually(t, "long's turn", func() string { return fmt.Sprint(strings.Contains(eval(`(call :jobs/list)`), "claude")) }, "true")
	if got := eval(`(call :long/stop)`); got != "(ok :done)" {
		t.Errorf("stop = %s", got)
	}
	if r, err := c.Message("wait", "", "["+id+"]", ""); err != nil || !strings.HasPrefix(r.Value, "(err {:kind :cancelled") {
		t.Errorf("a stopped turn = %+v %v", r, err)
	}
	if got := eval(`(call :long/stop)`); got != "(ok :idle)" {
		t.Errorf("stop with no turn = %s", got)
	}
}
