package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestState(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)

	dir := tempDir(t)
	counter := `(srv counter :state 0
  (on (get n) "The count." (ok n))
  (on (add n k) :update "Adds k." (ok [(+ n k) (+ n k)]))
  (on (refuse n) :update "Fails, keeping the count." (err {:kind :nope}))
  (on (boom n) :update "Faults." (+ 1 1.0))
  (on (leak n) :update "Puts a function in the state." (ok [1 (fn () n)]))
  (on (shape n) :update "Returns only the state." (ok n))
  (on :bump (n v) :update (ok (+ n v))))`
	writeServices(t, dir, map[string]string{
		"counter": counter,
		"started": `(srv started :state []
  :start (fn (s) (ok (vector/append s :started)))
  (on (get s) "The state." (ok s)))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// becomes waits until src evaluates to want.
	becomes := func(src, want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			resp, err := c.Eval(src, "")
			if err != nil {
				t.Fatal(err)
			}
			got = resp.Value + resp.Error
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s = %s, want %s", src, got, want)
			}
		}
	}
	kind := func(call string) string {
		return `(match ` + call + ` [(err {:kind k}) k] [(ok v) v])`
	}
	get := `(call :counter/get)`
	steps := []struct{ src, want string }{
		{get, `(ok 0)`},
		{`(call :counter/add 2)`, `(ok 2)`},
		{get, `(ok 2)`},
		// An err, a fault or a bad state keeps the state as it was.
		{`(call :counter/refuse)`, `(err {:kind :nope})`},
		{kind(`(call :counter/leak)`), `:fault`},
		{kind(`(call :counter/shape)`), `:fault`},
		{get, `(ok 2)`},
		// Callers don't pass the state.
		{kind(`(call :counter/add 1 2)`), `:bad-call`},
		{`(match (describe :counter) [(ok fs) (vector/filter fs (fn (f) (match f [{:fn :add} #t] [_ #f])))])`,
			`[{:doc "Adds k." :fn :add :form :on :mark :read :params [:k]}]`},
		{`(publish :bump 3)`, `(ok :done)`},
	}
	for _, s := range steps {
		becomes(s.src, s.want)
	}
	becomes(get, `(ok 5)`)

	// A fault restart starts from the last committed state.
	becomes(kind(`(call :counter/boom)`), `:fault`)
	historyHas(t, c, ":counter faulted; restarting it from its file")
	becomes(get, `(ok 5)`)

	// So does a reload, whatever init now says; :start runs again on it.
	becomes(`(call :started/get)`, `(ok [:started])`)
	reload := func(name, src string) {
		path := filepath.Join(dir, "services", name+".puff")
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Second)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	reload("counter", strings.Replace(strings.Replace(counter, ":state 0", ":state 100", 1),
		`(on (get n) "The count." (ok n))`, `(on (get n) "The count." (ok n)) (on (new n) "New." (ok :new))`, 1))
	becomes(`(call :counter/new)`, `(ok :new)`)
	becomes(get, `(ok 5)`)
	reload("started", `(srv started :state []
  :start (fn (s) (ok (vector/append s :again)))
  (on (get s) "The state." (ok s)))`)
	becomes(`(call :started/get)`, `(ok [:started :again])`)
}
