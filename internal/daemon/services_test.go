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
