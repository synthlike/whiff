package puff

import (
	"context"
	"errors"
	"testing"
)

// load loads src into env and returns the printed value, "<nil>" for none,
// or the error.
func load(env *Env, src string) string {
	return loadCtx(context.Background(), env, src)
}

func loadCtx(ctx context.Context, env *Env, src string) string {
	v, err := env.Load(ctx, "", src)
	if err != nil {
		return err.Error()
	}
	if v == nil {
		return "<nil>"
	}
	return Print(v)
}

const fact = "(def (fact n) (if (= n 0) 1 (* n (fact (- n 1))))) "

func TestEval(t *testing.T) {
	tests := []struct{ src, want string }{
		{"", "<nil>"},
		{"(def x 1)", "<nil>"},
		{"1", "1"},
		{"(+ 1 2 3)", "6"},
		{"(- 5)", "-5"},
		{"(- 5 7)", "-2"},
		{"(* 2 3 4)", "24"},
		{"(+ 1s 500ms)", "1500ms"},
		{"(* 1.5 2.0)", "3.0"},
		{"(def x 2) (def (sq n) (* n n)) (sq x)", "4"},
		{"(def (f) (g)) (def (g) 1) (f)", "1"},
		{"(let [a 1 b (+ a 1) a (* b 10)] [a b])", "[20 2]"},
		{"(let [x 1] (let [y 2] (let [z 3] [x y z])))", "[1 2 3]"},
		{"(def x 1) (let [x 2] x)", "2"},
		{"(def (adder n) (fn (x) (+ x n))) ((adder 2) 3)", "5"},
		{"(if (< 1 2) :yes :no)", ":yes"},
		{"(def (sign n) (cond [(< n 0) :neg] [(= n 0) :zero] [#t :pos])) [(sign -2) (sign 0) (sign 5)]", "[:neg :zero :pos]"},
		{"[(and #t #t) (and #t #f) (or #f #t) (or #f #f)]", "[#t #f #t #f]"},
		{"[(or #t 1) (and #f 1)]", "[#t #f]"},
		{"(do 1 2)", "2"},
		{"(ok [1 (some :a) (none)])", "(ok [1 (some :a) (none)])"},
		{"(let [k :a] {k 1 :b (+ 1 1)})", "{:a 1 :b 2}"},
		{"[(+ 1 1) [2]]", "[2 [2]]"},
		{"[(= [1 {:a 2}] [1 {:a 2}]) (= 1 1.0) (= + +)]", "[#t #f #t]"},
		{`[(< "a" "b") (>= 2s 2000ms) (not #f)]`, "[#t #t #t]"},
		{fact + "(fact 20)", "2432902008176640000"},
		{"(def (sq n) n) [sq (fn (x) x) +]", "[#<fn sq> #<fn> #<builtin +>]"},

		// Faults.
		{"(def x y) (def y 1)", "1:8: y used before its def"},
		{"(if 1 2 3)", "1:5: want a bool, got 1"},
		{"(and #t 1)", "1:9: want a bool, got 1"},
		{"(cond [#f 1])", "1:1: no cond arm matched"},
		{"(let [k :a] {k 1 :a 2})", "1:13: duplicate map key :a"},
		{fact + "(fact 21)", "1:29: *: int overflow"},
		{"(+ 9223372036854775807 1)", "1:1: +: int overflow"},
		{"(- -9223372036854775808)", "1:1: -: int overflow"},
		{"(* 1e300 1e300)", "1:1: *: float overflow"},
		{"(+ 1 1.0)", "1:1: +: can't add int and float"},
		{"(* 2s 2s)", "1:1: *: can't multiply duration and duration"},
		{"(< 1 1.0)", "1:1: <: can't compare int and float"},
		{"(not 1)", "1:1: not: want a bool, got 1"},
		{"(def (f n) (f n)) (f 1)", "1:12: too deep: more than 10000 nested calls"},
		{"(def (f x) x) (f)", "1:15: f takes 1 argument, got 0"},
		{"((fn (x y) x) 1)", "1:1: fn takes 2 arguments, got 1"},
		{"(not)", "1:1: not takes 1 argument, got 0"},
		{"(- 1 2 3)", "1:1: - takes 1 to 2 arguments, got 3"},
		{"(+ 1)", "1:1: + takes 2 or more arguments, got 1"},
		{"(1 2)", "1:1: not a function: 1"},

		// Errors found before anything runs.
		{"(foo 1)", "1:2: unknown name foo"},
		{"(def (f) (nope))", "1:11: unknown name nope"},
		{"(if #t 1)", "1:1: if takes a condition and two branches"},
		{"(let [a] a)", "1:1: let wants [name value ...] bindings and a body"},
		{"(let [a 1])", "1:1: let needs a body"},
		{"(fn (1) 1)", "1:6: want a name, got 1"},
		{"(fn (x x) x)", "1:8: parameter x twice"},
		{"(do (def x 1))", "1:5: def only at top level"},
		{"(def (f))", "1:1: def wants a name and a value, or (name params...) and a body"},
		{"(def x 1) (def x 2)", "1:16: x is already defined at 1:6"},
		{"(def + 1)", "1:6: can't redefine builtin +"},
		{"(def if 1)", "1:6: can't bind special form if"},
		{"(let [if 1] if)", "1:7: can't bind special form if"},
		{"if", "1:1: if is a special form, not a value"},
		{"()", "1:1: empty call ()"},
		{"(cond)", "1:1: cond needs arms"},
		{"(cond [#t])", "1:7: cond arms are [test body...]"},
		{"(and)", "1:1: and needs conditions"},
		{"(do)", "1:1: do needs a body"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestErrorKinds(t *testing.T) {
	var perr *Error
	if _, err := NewEnv().Load(context.Background(), "", "(nope)"); !errors.As(err, &perr) {
		t.Errorf("unknown name: got %T, want *Error", err)
	}
	var fault *Fault
	if _, err := NewEnv().Load(context.Background(), "", "(+ 1 1.0)"); !errors.As(err, &fault) {
		t.Errorf("bad add: got %T, want *Fault", err)
	}
}

func TestEnvAcrossLoads(t *testing.T) {
	env := NewEnv()
	steps := []struct{ src, want string }{
		{"(def x 1) (def (f) x)", "<nil>"},
		{"(f)", "1"},
		{"(def x 2)", "<nil>"},
		{"(f)", "2"}, // redefining x changes what f sees
		{"(def a 1) (nope)", "1:12: unknown name nope"},
		{"a", "1:1: unknown name a"}, // a load that fails its check defines nothing
		{"(def b 1) (+ b 1.0) (def c 2)", "1:11: +: can't add int and float"},
		{"b", "1"}, // a fault keeps the defs that ran
		{"c", "1:1: c used before its def"},
	}
	for _, s := range steps {
		if got := load(env, s.src); got != s.want {
			t.Errorf("%s\n got %s\nwant %s", s.src, got, s.want)
		}
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, want := loadCtx(ctx, NewEnv(), "(+ 1 2)"), "1:1: cancelled"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
