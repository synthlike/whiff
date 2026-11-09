package huff

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, l *Local, fn string, args map[string]any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	return l.Call(context.Background(), Call{ID: "1", Fn: fn, Args: string(b)})
}

func TestLocalFiles(t *testing.T) {
	dir := t.TempDir()
	l := &Local{Dir: dir, Names: LocalTools}

	if out, isErr := call(t, l, "Write", map[string]any{"file_path": "cmd/x/main.go", "content": "package main\n\nfunc main() {}\n"}); isErr || out != "wrote cmd/x/main.go, 3 lines" {
		t.Fatalf("Write = %q %v", out, isErr)
	}
	if out, _ := call(t, l, "Read", map[string]any{"file_path": filepath.Join(dir, "cmd/x/main.go"), "offset": 3}); out != "     3\tfunc main() {}\n" {
		t.Errorf("Read from line 3 = %q", out)
	}
	if out, _ := call(t, l, "Read", map[string]any{"file_path": "cmd/x/main.go", "limit": 1}); !strings.Contains(out, "(2 more lines: read on from offset 2)") {
		t.Errorf("Read a line = %q", out)
	}
	if out, isErr := call(t, l, "Edit", map[string]any{"file_path": "cmd/x/main.go", "old_string": "func main() {}", "new_string": "func main() { println(1) }"}); isErr {
		t.Errorf("Edit = %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cmd/x/main.go")); !strings.Contains(string(b), "println(1)") {
		t.Errorf("the edit isn't in the file: %s", b)
	}
	if out, isErr := call(t, l, "Edit", map[string]any{"file_path": "cmd/x/main.go", "old_string": "nothing like it", "new_string": "x"}); !isErr || !strings.Contains(out, "isn't in cmd/x/main.go") {
		t.Errorf("Edit of text that isn't there = %q %v", out, isErr)
	}
	call(t, l, "Write", map[string]any{"file_path": "a.txt", "content": "x\nx\n"})
	if out, isErr := call(t, l, "Edit", map[string]any{"file_path": "a.txt", "old_string": "x", "new_string": "y"}); !isErr || !strings.Contains(out, "2 times") {
		t.Errorf("Edit of text there twice = %q %v", out, isErr)
	}
	if out, isErr := call(t, l, "Edit", map[string]any{"file_path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": true}); isErr || out != "edited a.txt: 2 replaced" {
		t.Errorf("Edit replace_all = %q %v", out, isErr)
	}
	if out, _ := call(t, l, "Glob", map[string]any{"pattern": "**/*.go"}); out != "cmd/x/main.go" {
		t.Errorf("Glob **/*.go = %q", out)
	}
	if out, _ := call(t, l, "Glob", map[string]any{"pattern": "*.txt"}); out != "a.txt" {
		t.Errorf("Glob *.txt = %q", out)
	}
	if out, _ := call(t, l, "Grep", map[string]any{"pattern": "println", "glob": "*.go"}); out != "cmd/x/main.go:3: func main() { println(1) }" {
		t.Errorf("Grep = %q", out)
	}
}

func TestLocalStaysInDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))
	l := &Local{Dir: dir, Names: []string{"Read", "Write"}}
	for _, p := range []string{"../x", filepath.Join(outside, "secret"), "link/secret", "link/new"} {
		if out, isErr := call(t, l, "Read", map[string]any{"file_path": p}); !isErr || !strings.Contains(out, "outside") {
			t.Errorf("Read %s = %q %v, want it refused", p, out, isErr)
		}
		if out, isErr := call(t, l, "Write", map[string]any{"file_path": p, "content": "x"}); !isErr || !strings.Contains(out, "outside") {
			t.Errorf("Write %s = %q %v, want it refused", p, out, isErr)
		}
	}
	if out, isErr := call(t, l, "Glob", map[string]any{"pattern": "*"}); !isErr || out != "there is no tool Glob" {
		t.Errorf("a tool it doesn't offer = %q %v", out, isErr)
	}
}

func TestLocalBash(t *testing.T) {
	dir := t.TempDir()
	l := &Local{Dir: dir, Names: []string{"Bash"}, Allow: []string{"echo hi", "ls:*"}}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "echo   hi"}); isErr || out != "hi\n" {
		t.Errorf("an allowed command = %q %v", out, isErr)
	}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "ls -a"}); isErr || !strings.Contains(out, ".") {
		t.Errorf("an allowed prefix = %q %v", out, isErr)
	}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "ls /nowhere-at-all"}); !isErr || !strings.Contains(out, "(exit ") {
		t.Errorf("a command that fails = %q %v", out, isErr)
	}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "lsof"}); !isErr || !strings.Contains(out, "isn't allowed") {
		t.Errorf("a prefix's lookalike = %q %v", out, isErr)
	}
	for _, c := range []string{"echo hi; rm -rf x", "echo hi | cat", "echo $HOME", "ls > out"} {
		if out, isErr := call(t, l, "Bash", map[string]any{"command": c}); !isErr || !strings.Contains(out, "needs a shell") {
			t.Errorf("%s = %q %v, want it refused", c, out, isErr)
		}
	}
	if got := strings.Join(l.Denied, "|"); got != "lsof" {
		t.Errorf("Denied = %q", got)
	}
	if w, err := words(`go test -run 'TestA B' "./..."`); err != nil || strings.Join(w, "|") != "go|test|-run|TestA B|./..." {
		t.Errorf("words = %q %v", w, err)
	}
}

// wrap is a Runner that runs each command through echo, so its output is
// the command it was given and where.
type wrap struct{}

func (wrap) Command(ctx context.Context, dir string, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, "echo", append([]string{"in", dir + ":"}, argv...)...)
}

// TestLocalRunner: Bash's allowed commands go through its Runner, as a
// sandbox makes them; refused ones never reach it.
func TestLocalRunner(t *testing.T) {
	dir := t.TempDir()
	l := &Local{Dir: dir, Names: []string{"Bash"}, Allow: []string{"go test:*"}, Runner: wrap{}}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "go test ./..."}); isErr || out != "in "+dir+": go test ./...\n" {
		t.Errorf("through the runner = %q %v", out, isErr)
	}
	if out, isErr := call(t, l, "Bash", map[string]any{"command": "rm -rf /"}); !isErr || !strings.Contains(out, "isn't allowed") {
		t.Errorf("a refused command = %q %v", out, isErr)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"**/*.go", "main.go", true}, {"**/*.go", "a/b/c.go", true}, {"*.go", "a/b.go", false},
		{"cmd/**", "cmd/x/y", true}, {"cmd/*/main.go", "cmd/x/main.go", true}, {"a/**/b", "a/b", true},
	} {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}
