package puff

import "testing"

func TestTry(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(def (f r) (let [x (try r)] (ok (+ x 1)))) [(f (ok 1)) (f (err :bad))]", "[(ok 2) (err :bad)]"},
		{
			"(def (inner) (err {:kind :x}))\n(def (mid) (try (inner)))\n(def (top) (try (mid)))\n(top)",
			`(err {:kind :x :trace ["2:12" "3:12"]})`,
		},
		{`(def (parse s) (if (= s "1") (ok 1) (err :nan))) (def (f s) (-> s (try parse) (+ 1) ok)) [(f "1") (f "x")]`, "[(ok 2) (err :nan)]"},
		{"(def (f) (loop [i 0] (if (= i 3) (ok i) (do (try (if (< i 2) (ok i) (err i))) (recur (+ i 1)))))) (f)", "(err 2)"},
		{"(def (f) (try (ok 1)) (ok 2)) (f)", "(ok 2)"},
		{"(err 1)", "(err 1)"},

		{"(def (f) (try 1)) (f)", "1:10: try wants (ok v) or (err e), got 1"},
		{"(def (f) (err :x) 1) (f)", "1:10: dropped an err: (err :x)"},
		{"(let [a 1] (err a) a)", "1:12: dropped an err: (err 1)"},
		{"(err 1) 2", "1:1: dropped an err: (err 1)"},
		{`(fail "boom")`, "1:1: fail: boom"},
		{"(fail {:kind :x})", "1:1: fail: {:kind :x}"},

		{"(try (ok 1))", "1:1: try only inside a function"},
		{"(def (f) (try))", "1:10: try takes one Result"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestCatch(t *testing.T) {
	tests := []struct{ src, want string }{
		// An err an arm matches is handled; the rest go on as they are.
		{`(def (f r) (catch r [{:kind :not-found} (ok "")])) [(f (err {:kind :not-found})) (f (err {:kind :io})) (f (ok "x"))]`,
			`[(ok "") (err {:kind :io}) (ok "x")]`},
		{`[(catch (err 1) [e (ok (+ e 1))]) (catch (some 1) [_ 2]) (catch 5 [_ 2])]`, `[(ok 2) (some 1) 5]`},
		{`(catch (err {:kind :x :n 3}) [{:n n} when (> n 5) :big] [{:n n} [:small n]])`, `[:small 3]`},
		{`(catch (err {:kind :x}) [{:kind :y} 1] [{:kind :x} 2])`, `2`},
		// With try: handle one kind here, pass the rest up.
		{`(def (read k) (err {:kind k})) (def (f k) (let [s (try (catch (read k) [{:kind :not-found} (ok "")]))] (ok (str s "!")))) [(f :not-found) (f :io)]`,
			`[(ok "!") (err {:kind :io :trace ["1:51"]})]`},
		// An arm's body is in tail position.
		{`(loop [i 0] (catch (if (< i 3) (err i) (ok i)) [n (recur (+ n 1))]))`, `(ok 3)`},

		{`(catch (err 1))`, "1:1: catch wants a value and [pattern body...] arms"},
		{`(catch (err 1) 2)`, "1:16: catch arms are [pattern body...]"},
		{`(catch (err 1) [_ (err 2) 3])`, "1:19: dropped an err: (err 2)"},
		{`catch`, "1:1: catch is a special form, not a value"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestMapWith(t *testing.T) {
	m := mustMap(t, Keyword("b"), Int(1))
	m2, err := m.With(Keyword("a"), Int(2))
	if err != nil {
		t.Fatal(err)
	}
	m3, err := m2.With(Keyword("b"), Int(3))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Print(m3), "{:a 2 :b 3}"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got, want := Print(m), "{:b 1}"; got != want {
		t.Errorf("With changed its map: %s, want %s", got, want)
	}
	if _, err := m.With(Float(1), Int(1)); err == nil {
		t.Error("With(1.0) succeeded, want an error")
	}
}
