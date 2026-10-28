package daemon

import (
	"strings"
	"testing"
)

func TestTry(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(service greet (on (hello name) "Greets name." (ok "installed")))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// The file being tried is another greet, beside the installed one.
	greet := `(service greet
  (def (shout s) (string/concat s "!"))
  (on (hello name) "Greets name." (ok (shout (string/concat "hello, " name))))
  (on (twice name) "Greets name through its own hello." (hello name))
  (on (relay name) "Greets name through the installed greet." (call :greet/hello name))
  (ask (later x) "Waits."))`
	tests := []struct{ fn, args, want string }{
		{"hello", `["ann"]`, `(ok "hello, ann!")`},
		{":hello", `["ann"]`, `(ok "hello, ann!")`},
		{"twice", `["bob"]`, `(ok "hello, bob!")`},
		{"relay", `["cat"]`, `(ok "installed")`},
		{"hello", `[]`, `(err {:kind :bad-call :message ":try/5/greet/hello takes 1 arguments, got 0"})`},
		{"nope", `[]`, `(err {:kind :no-function :message ":try/6/greet has no function nope"})`},
		{"later", `[1]`, `(err {:id 1 :kind :pending :message ":try/7/greet/later waits in :try/7/greet's inbox for a reply"})`},
	}
	for _, tt := range tests {
		resp, err := c.Try("greet.puff", greet, tt.fn, tt.args, "/tmp")
		if err != nil || resp.Error != "" || resp.Value != tt.want {
			t.Errorf("try %s %s = %+v %v, want %s", tt.fn, tt.args, resp, err, tt.want)
		}
	}
	historyHas(t, c, `  call  user   :try/4/greet -> :greet/hello "cat"  (ok "installed")`)
	// What waited in the trial's inbox is answered when it goes.
	historyHas(t, c, `  done  user   :try/7/greet -> client 1 (err {:kind :no-address :message "no address :try/7/greet"})`)

	// The installed greet is untouched, and the trials are gone.
	for src, want := range map[string]string{
		`(call :greet/hello "x")`:                                   `(ok "installed")`,
		`(match (call :try/1/greet/hello "x") [(err {:kind k}) k])`: `:no-address`,
	} {
		if resp, err := c.Eval(src, ""); err != nil || resp.Value != want {
			t.Errorf("%s = %+v %v, want %s", src, resp, err, want)
		}
	}

	bad := []struct{ file, src, want string }{
		{"greet.puff", `(service greet (on (hello) "Doc." (ok x)))`, "greet.puff:1:39: unknown name x"},
		{"x/oops.puff", `(service greet)`, "x/oops.puff:1:1: service wants its name, oops, as in its file name"},
		{"greet.puff", `(service greet`, "greet.puff:1:1: no closing ')'"},
	}
	for _, tt := range bad {
		if resp, err := c.Try(tt.file, tt.src, "hello", "[]", ""); err != nil || resp.Error != tt.want {
			t.Errorf("try %s = %+v %v, want error %q", tt.src, resp, err, tt.want)
		}
	}
	if resp, err := c.Try("greet.puff", `(service greet (on (where) "Its address." (ok (self))))`, "where", "[]", ""); err != nil || !strings.HasPrefix(resp.Value, "(ok :try/") || !strings.HasSuffix(resp.Value, "/greet)") {
		t.Errorf("(self) in a trial = %+v %v", resp, err)
	}
	if resp, err := c.Eval("(self)", ""); err != nil || resp.Error != "eval:1:1: self: only a service, or a client that holds an address, has one" {
		t.Errorf("(self) from a client = %+v %v", resp, err)
	}
	if resp, err := c.Try("greet.puff", greet, "hello", "nope", ""); err != nil || resp.Error != "want arguments as a vector, got nope" {
		t.Errorf("try with bad args = %+v %v", resp, err)
	}

	// No client can take an address a trial might use.
	other := dial(t, dir)
	for _, addr := range []string{"try", "try/99/greet"} {
		if resp, err := other.Claim(addr, ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:kind :taken") {
			t.Errorf("claim :%s = %+v %v, want :taken", addr, resp, err)
		}
	}
}

func TestTestOp(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)

	// Its tests reach its defs, its handlers and whiff's builtins; its :start
	// doesn't run.
	src := `(service counter :state 0
  :start (fn (state) (fail "started"))
  (def (bump n) (+ n 1))
  (on (add state n) :update "Adds n." (ok [:added (+ state n)]))
  (test "bump" (= (bump 1) 2))
  (test "a handler" (= (add 1 2) (ok [:added 3])))
  (test "its address" (string/starts-with? (value/to-string (self)) ":try/"))
  (test "where it runs" (= (os/cwd) "/tmp"))
  (test "wrong" (= (bump 1) 3)))`
	resp, err := c.Test("counter.puff", src, "/tmp")
	want := `[{:at "counter.puff:5:3" :message "" :test "bump"} {:at "counter.puff:6:3" :message "" :test "a handler"} ` +
		`{:at "counter.puff:7:3" :message "" :test "its address"} {:at "counter.puff:8:3" :message "" :test "where it runs"} ` +
		`{:at "counter.puff:9:3" :message "got 2, want 3" :test "wrong"}]`
	if err != nil || resp.Error != "" || resp.Value != want {
		t.Errorf("test = %+v %v, want %s", resp, err, want)
	}
	// whiff try runs its :start, which test didn't.
	if resp, err := c.Try("counter.puff", src, "add", "[2]", ""); err != nil || resp.Error != "counter.puff:2:22: fail: started" {
		t.Errorf("try = %+v %v", resp, err)
	}

	if resp, err := c.Test("counter.puff", `(service counter (test "x" (nope)))`, ""); err != nil || resp.Error != "counter.puff:1:29: unknown name nope" {
		t.Errorf("a broken file = %+v %v", resp, err)
	}
	agent := dial(t, dir)
	agent.Agent = true
	if resp, err := agent.Test("counter.puff", src, ""); err != nil || resp.Error != "an agent's connection can't run tests" {
		t.Errorf("an agent's test = %+v %v", resp, err)
	}
	if resp, err := agent.Try("counter.puff", src, "add", "[2]", ""); err != nil || resp.Error != "an agent's connection can't try files" {
		t.Errorf("an agent's try = %+v %v", resp, err)
	}
}
