package puff

import (
	"context"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args     string
		min, max int
	}{
		{"", 0, 0},
		{"v", 1, 1},
		{"s sep", 2, 2},
		{"a [b]", 1, 2},
		{"path [opts]", 1, 2},
		{"a b ...", 2, -1},
	}
	for _, tt := range tests {
		if min, max := parseArgs(tt.args); min != tt.min || max != tt.max {
			t.Errorf("parseArgs(%q) = %d, %d; want %d, %d", tt.args, min, max, tt.min, tt.max)
		}
	}
}

func TestRegister(t *testing.T) {
	env := NewEnv()
	if err := env.Register(&Builtin{Name: "+", Args: "a b"}); err == nil {
		t.Error("registering + again succeeded, want an error")
	}
}

func TestErrorf(t *testing.T) {
	if got, want := Print(Errorf("nan", "not a number: %q", "x")), `(err {:kind :nan :message "not a number: \"x\""})`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestHelp(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(help +)", "(+ a b ...) :read\n  The sum of ints, floats or durations, or a time plus durations.\n"},
		{"(help none)", "(none) :read\n  An empty Option.\n"},
		{"(def (sq n) (* n n)) (help sq)", "(sq n) :read\n"},
		{"(help (fn (x y) x))", "(fn x y) :read\n"},
	}
	for _, tt := range tests {
		env := NewEnv()
		var out strings.Builder
		env.Stdout = &out
		if got := load(env, tt.src); got != "(ok :done)" {
			t.Errorf("%s = %s, want (ok :done)", tt.src, got)
		}
		if out.String() != tt.want {
			t.Errorf("%s printed %q, want %q", tt.src, out.String(), tt.want)
		}
	}

	env := NewEnv()
	var out strings.Builder
	env.Stdout = &out
	load(env, "(def (f) 1) (help)")
	if !strings.Contains(out.String(), "(* a b ...) :read\n(+ a b ...) :read\n") || strings.Contains(out.String(), "(f)") {
		t.Errorf("(help) printed %q, want every builtin and no defs", out.String())
	}
	if got, want := load(NewEnv(), "(help 1)"), "1:1: help: want a function, got 1"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// writeEnv is an Env with a :write builtin, test/write.
func writeEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv()
	err := env.Register(&Builtin{Name: "test/write", Args: "v", Mark: MarkWrite, Fn: func(_ *Call, args []Value) (Value, error) {
		return Done, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func markOf(t *testing.T, env *Env, src string) Mark {
	t.Helper()
	v, err := env.Load(context.Background(), "", src)
	if err != nil {
		t.Fatal(err)
	}
	return MarkOf(v)
}

func TestMarkOf(t *testing.T) {
	tests := []struct {
		src  string
		want Mark
	}{
		{"+", MarkRead},
		{"test/write", MarkWrite},
		{"(def (a) 1) a", MarkRead},
		{"(def (b) (test/write 1)) b", MarkWrite},
		{"(def (b) (test/write 1)) (def (c) (b)) c", MarkWrite},
		{"(def (p f) (f 1)) p", MarkRead},
		{"(def (p f) (f 1)) (def (q) (p test/write)) q", MarkWrite},
		{"(def w test/write) (def (r) (w 1)) r", MarkWrite},
		{"(def f (let [w test/write] (fn () (w 1)))) f", MarkWrite},
		{"(def fs [test/write]) (def (g) fs) g", MarkWrite},
		{"(def (mk) (fn () (test/write 1))) mk", MarkWrite},
		{"(def (m n) (if (= n 0) 0 (k n))) (def (k n) (m (- n 1))) m", MarkRead},
		{"(def (m n) (if (= n 0) 0 (k n))) (def (k n) (do (test/write n) (m (- n 1)))) m", MarkWrite},
		{"[1 {:f test/write}]", MarkWrite},
		{"1", MarkRead},
	}
	for _, tt := range tests {
		if got := markOf(t, writeEnv(t), tt.src); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.src, got, tt.want)
		}
	}
}

// Marks are worked out when asked, so redefining a helper changes its users.
func TestMarkFollowsRedefinition(t *testing.T) {
	env := writeEnv(t)
	if got := markOf(t, env, "(def (h) 1) (def (u) (h)) u"); got != MarkRead {
		t.Fatalf("u is %s before, want :read", got)
	}
	if got := markOf(t, env, "(def (h) (test/write 1)) u"); got != MarkWrite {
		t.Errorf("u is %s after h writes, want :write", got)
	}
}

func TestWithMark(t *testing.T) {
	file := t.TempDir() + "/f"
	read := WithMark(context.Background(), ":a/f", MarkRead)
	tests := []struct {
		ctx       context.Context
		src, want string
	}{
		{read, `(match (file/read-text "` + file + `") [(err {:kind k}) k])`, `:not-found`},
		{read, `(file/write "` + file + `" "x")`, `1:1: file/write is :write, and :a/f is :read`},
		{read, `(vector/map ["x"] (fn (s) (file/write "` + file + `" s)))`, `1:27: file/write is :write, and :a/f is :read`},
		{WithMark(read, ":a/g", MarkWrite), `(file/write "` + file + `" "x")`, `(ok :done)`},
		// proc/run runs a read-only command under :read, and only that.
		{read, `(match (proc/run ["pwd"] {:dir "/"}) [(ok {:stdout out}) out])`, `"/\n"`},
		{read, `(proc/output ["git" "push"])`, `1:1: proc/output: ["git" "push"] isn't a read-only command, and :a/f is :read`},
	}
	for _, tt := range tests {
		if got := loadCtx(tt.ctx, NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.src, got, tt.want)
		}
	}
	if what, ok := ReadOnly(read); what != ":a/f" || !ok {
		t.Errorf("ReadOnly = %q %v", what, ok)
	}
}
