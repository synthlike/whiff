package puff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// files writes name→src under a new directory and returns it.
func files(t *testing.T, fs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range fs {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadFile loads dir/name as puff file.puff does: the printed value, or
// the error.
func loadFile(t *testing.T, env *Env, dir, name string) string {
	t.Helper()
	file := filepath.Join(dir, name)
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	v, err := env.LoadFile(context.Background(), file, string(src))
	switch {
	case err != nil:
		return strings.ReplaceAll(err.Error(), dir+"/", "")
	case v == nil:
		return "<nil>"
	}
	return Print(v)
}

func TestUse(t *testing.T) {
	dir := files(t, map[string]string{
		"main.puff":       `(use "lib/a.puff") (use "lib/b.puff") (double (inc 1))`,
		"lib/a.puff":      `(use "b.puff") (def (double n) (* 2 (inc n)))`,
		"lib/b.puff":      `(def (inc n) (+ n 1)) (test "inc" (= (inc 1) 2))`,
		"cycle.puff":      `(use "lib/c.puff") 1`,
		"lib/c.puff":      `(use "d.puff")`,
		"lib/d.puff":      "\n  (use \"c.puff\")",
		"self.puff":       `(use "lib/self.puff")`,
		"lib/self.puff":   `(use "../self.puff")`,
		"expr.puff":       `(use "lib/expr.puff")`,
		"lib/expr.puff":   "(def x 1)\n(+ x 1)",
		"twice.puff":      `(use "lib/b.puff") (def (inc n) n)`,
		"missing.puff":    "1\n(use \"lib/none.puff\")",
		"notstr.puff":     `(use lib)`,
		"inner.puff":      `(def (f) (use "lib/b.puff"))`,
		"bad.puff":        `(use "lib/bad.puff")`,
		"lib/bad.puff":    `(def (f) (nope))`,
		"unread.puff":     `(use "lib/unread.puff")`,
		"lib/unread.puff": `(def x [1 2`,
	})
	tests := []struct{ file, want string }{
		// b comes in once, though both main and a use it.
		{"main.puff", "6"},
		{"cycle.puff", "lib/d.puff:2:3: a cycle of uses: lib/c.puff uses lib/d.puff uses lib/c.puff"},
		{"self.puff", "lib/self.puff:1:1: a cycle of uses: self.puff uses lib/self.puff uses self.puff"},
		{"expr.puff", "lib/expr.puff:2:1: a used file holds only defs, tests and uses"},
		{"twice.puff", "twice.puff:1:26: inc is already defined at lib/b.puff:1:7"},
		{"missing.puff", "missing.puff:2:1: can't use lib/none.puff: no such file or directory"},
		{"notstr.puff", `notstr.puff:1:1: use wants a file's path, as a string`},
		{"inner.puff", "inner.puff:1:10: use only at top level"},
		{"bad.puff", "lib/bad.puff:1:11: unknown name nope"},
		{"unread.puff", "lib/unread.puff:1:8: no closing ']'"},
	}
	for _, tt := range tests {
		if got := loadFile(t, NewEnv(), dir, tt.file); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.file, got, tt.want)
		}
	}
}

func TestUseRunsUsedTests(t *testing.T) {
	dir := files(t, map[string]string{
		"main.puff":  `(use "lib/b.puff") (test "main" (= (inc 2) 3))`,
		"lib/b.puff": `(def (inc n) (+ n 1)) (test "inc" (= (inc 1) 2))`,
	})
	env := NewEnv()
	if got := loadFile(t, env, dir, "main.puff"); got != "<nil>" {
		t.Fatal(got)
	}
	var names []string
	for _, tt := range env.Tests() {
		names = append(names, tt.Name)
		if msg := env.RunTest(context.Background(), tt); msg != "" {
			t.Errorf("%s: %s", tt.Name, msg)
		}
	}
	if got := strings.Join(names, " "); got != "inc main" {
		t.Errorf("tests %q, want \"inc main\"", got)
	}
}

// A symlinked file's uses start from its target's directory: a service
// linked into ~/.whiff/services uses the lib next to its source.
func TestUseFromSymlink(t *testing.T) {
	dir := files(t, map[string]string{
		"src/main.puff":  `(use "lib/b.puff") (inc 1)`,
		"src/lib/b.puff": `(def (inc n) (+ n 1))`,
	})
	if err := os.Mkdir(filepath.Join(dir, "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "src", "main.puff"), filepath.Join(dir, "linked", "main.puff")); err != nil {
		t.Fatal(err)
	}
	if got := loadFile(t, NewEnv(), dir, "linked/main.puff"); got != "2" {
		t.Errorf("got %s, want 2", got)
	}
}

// Without a file, as in a REPL, uses start from e.Dir.
func TestUseFromDir(t *testing.T) {
	dir := files(t, map[string]string{"lib/b.puff": `(def (inc n) (+ n 1))`})
	env := NewEnv()
	env.Dir = dir
	if got := load(env, `(use "lib/b.puff") (inc 1)`); got != "2" {
		t.Errorf("got %s, want 2", got)
	}
}

// A used def is :write if what it reaches is, as a def of the file's own.
func TestUseMarks(t *testing.T) {
	dir := files(t, map[string]string{
		"main.puff":  `(use "lib/w.puff") (def (f) (save "x")) (def (g) (inc 1))`,
		"lib/w.puff": `(def (save s) (file/write "out" s)) (def (inc n) (+ n 1))`,
	})
	env := NewEnv()
	if got := loadFile(t, env, dir, "main.puff"); got != "<nil>" {
		t.Fatal(got)
	}
	for name, want := range map[string]Mark{"f": MarkWrite, "g": MarkRead} {
		v, _ := env.Lookup(name)
		if got := MarkOf(v); got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
}
