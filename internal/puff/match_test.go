package puff

import (
	"context"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(match 2 [1 :one] [2 :two] [_ :many])", ":two"},
		{"(match :k [:j 1] [:k 2])", "2"},
		{"(match 1.0 [1 :int] [1.0 :float])", ":float"},
		{"(match (ok 5) [(ok n) n] [(err e) e])", "5"},
		{"(match (none) [(some x) x] [(none) 0])", "0"},
		{"(match (ok 1) [(ok) :empty] [(ok x) x])", "1"},
		{"(match [1 2 3] [[a b & rest] [a b rest]])", "[1 2 [3]]"},
		{"(match [1] [[a b] :two] [[a] a])", "1"},
		{"(match [1 2] [[a] :one] [[_ & r] r])", "[2]"},
		{"(match {:a 1 :b 2} [{:a x :c y} [x y]] [{:a x} x])", "1"},
		{`(match (ok {:user {:name "ann"}}) [(ok {:user {:name n}}) n])`, `"ann"`},
		{"(let [x 1] (match 2 [x x]))", "2"},
		{"(def (sign n) (match n [0 :zero] [x when (< x 0) :neg] [_ :pos])) [(sign -1) (sign 0) (sign 3)]", "[:neg :zero :pos]"},

		{"(match 3 [1 :one])", "1:1: no match for 3"},
		{"(match 1 [x when x x])", "1:18: want a bool, got 1"},

		{"(match 1)", "1:1: match wants a value and [pattern body...] arms"},
		{"(match 1 [x])", "1:10: match arms are [pattern body...]"},
		{"(match 1 [x when #t])", "1:10: match arms with a guard are [pattern when test body...]"},
		{"(match [1] [[a & b c] a])", "1:16: & takes one pattern, at the end"},
		{"(match 1 [[x x] x])", "1:14: x bound twice in pattern"},
		{"(match 1 [(1 x) x])", "1:11: tag patterns are (name patterns...)"},
		{"(match {} [{k 1} 1])", "1:13: map pattern keys must be literals"},
		{"(match 1 [& 1])", "1:11: & only goes in vector patterns, before the rest"},
		{"(match 1 [if 1])", "1:11: can't bind special form if"},
		{"(when #t 1)", "1:1: when only goes in a match arm: [pattern when test body...]"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestLoop(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(loop [i 0 acc 0] (if (= i 5) acc (recur (+ i 1) (+ acc i))))", "10"},
		{"(loop [i 0] (if (= i 100000) i (recur (+ i 1))))", "100000"}, // no stack growth
		{"(loop [i 0] (let [j (+ i 1)] (cond [(< j 3) (do (recur j))] [#t (match j [3 :done])])))", ":done"},
		{"(def (sum n) (loop [i 0 acc 0] (if (> i n) acc (recur (+ i 1) (+ acc i))))) (sum 10)", "55"},

		{"(recur 1)", "1:1: recur outside a loop"},
		{"(loop [i 0] (+ 1 (recur i)))", "1:18: recur only in tail position"},
		{"(loop [i 0] (if (recur 1) 1 2))", "1:17: recur only in tail position"},
		{"(loop [i 0] (recur 1 2))", "1:13: recur takes 1 argument, got 2"},
		{"(loop [i 0] (fn () (recur 1)))", "1:20: recur outside a loop"},
		{"(loop [a] a)", "1:1: loop wants [name value ...] bindings and a body"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestLoopCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, want := loadCtx(ctx, NewEnv(), "(loop [i 0] (recur i))"), "1:1: cancelled"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestThread(t *testing.T) {
	tests := []struct{ src, want string }{
		{"(-> 5 (- 3) (* 2))", "4"},
		{"(def (inc n) (+ n 1)) (-> 1 inc inc)", "3"},
		{"(-> 1)", "1"},
		{"(-> 1 (+ 1.0))", "1:7: +: can't add int and float"},
		{"(-> 1 2)", "1:7: -> steps are names or calls, got 2"},
		{"(->)", "1:1: -> wants a value and steps"},
	}
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}
