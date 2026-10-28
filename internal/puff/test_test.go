package puff

import (
	"context"
	"testing"
)

func TestTests(t *testing.T) {
	env := NewEnv()
	src := `(def (sq n) (* n n))
(test "squares" (= (sq 3) 9))
(test "wrong" (= (sq 3) 10))
(test "a bool" (< (sq 2) 5))
(test "not #t" (sq 2))
(test "a fault" (= (sq "x") 1))
(def (parse s) (if (= s "1") (ok 1) (err {:kind :nan})))
(test "try" (= (try (parse "x")) 1))
(test "lets and dos" (let [x (sq 2)] (= x 4)))
(test "a later def" (= (cube 2) 8))
(def (cube n) (* n (sq n)))
:loaded`
	v, err := env.Load(context.Background(), "t.puff", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := Print(v); got != ":loaded" {
		t.Errorf("load = %s, want :loaded", got)
	}
	want := []struct{ name, pos, msg string }{
		{"squares", "t.puff:2:1", ""},
		{"wrong", "t.puff:3:1", "got 9, want 10"},
		{"a bool", "t.puff:4:1", ""},
		{"not #t", "t.puff:5:1", "got 4, want #t"},
		{"a fault", "t.puff:6:1", "t.puff:1:13: *: can't multiply string and string"},
		{"try", "t.puff:8:1", `got (err {:kind :nan :trace ["t.puff:8:16"]})`},
		{"lets and dos", "t.puff:9:1", ""},
		{"a later def", "t.puff:10:1", ""},
	}
	tests := env.Tests()
	if len(tests) != len(want) {
		t.Fatalf("%d tests, want %d", len(tests), len(want))
	}
	for i, w := range want {
		tt := tests[i]
		if tt.Name != w.name || tt.Pos.String() != w.pos {
			t.Errorf("test %d is %q at %s, want %q at %s", i, tt.Name, tt.Pos, w.name, w.pos)
		}
		if got := env.RunTest(context.Background(), tt); got != w.msg {
			t.Errorf("%s: %q, want %q", tt.Name, got, w.msg)
		}
	}

	for src, want := range map[string]string{
		`(test "x")`:              "1:1: test wants a name and a body",
		`(test x 1)`:              "1:1: test wants a name and a body",
		`(test :x #t)`:            "1:7: test wants its name as a string, got :x",
		`(def (f) (test "x" #t))`: "1:10: test only at top level",
		`(test "x" (nope))`:       "1:12: unknown name nope",
		`(def test 1)`:            "1:6: can't bind special form test",
	} {
		if got := load(NewEnv(), src); got != want {
			t.Errorf("%s: %s, want %s", src, got, want)
		}
	}
}
