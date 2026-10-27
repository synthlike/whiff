package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	tests := []struct{ fn, dir, want string }{
		{"branch", repo, `(ok "main")`},
		{"status", repo, `(ok {:ahead 0 :behind 0 :branch (some "main") :files [` +
			`{:index :none :path "a.txt" :work :modified} ` +
			`{:index :renamed :path "new name.txt" :work :none} ` +
			`{:index :added :path "new.txt" :work :none} ` +
			`{:index :untracked :path "loose file.txt" :work :untracked}] :upstream (none)})`},
		{"branch", dir, `(err {:exit 128 :kind :git :message "fatal: not a git repository (or any of the parent directories): .git" :trace ["git.puff:52:25"]})`},
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
