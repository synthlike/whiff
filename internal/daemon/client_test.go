package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestClient(t *testing.T) {
	set(t, &defaultCallTimeout, 500*time.Millisecond)

	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"relay": `(service relay
  (on (tell text) "Tells hiss." (call :hiss/toast text)))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// hiss holds :hiss and answers each toast with what it got.
	hiss := dial(t, dir)
	resp, err := hiss.Claim(":hiss", `(on (toast text) "Shows text.")
(on (fail) "Answers with an err.")
(on (silent) "Never answers.")
(on (whiff/done id r) "Gets answers.")
(ask (review x) :write "Waits for a person.")`)
	if err != nil || resp.Value != "(ok :done)" {
		t.Fatalf("claim :hiss = %+v %v", resp, err)
	}
	go func() {
		for d := range hiss.Deliveries() {
			switch d.Fn {
			case "toast":
				hiss.Answer(d.ID, fmt.Sprintf("(ok %q)", d.Args+" from "+d.From+" as "+d.Origin))
			case "fail":
				hiss.Answer(d.ID, `(err {:kind :nope})`)
			case "whiff/done":
				hiss.Answer(d.ID, `(ok :thanks)`)
			}
		}
	}()

	eval := func(c *rpc.Client, src string) string {
		t.Helper()
		resp, err := c.Eval(src, "")
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	kind := func(call string) string {
		return `(match ` + call + ` [(err {:kind k}) k] [(ok v) v])`
	}
	tests := []struct{ src, want string }{
		{`(call :hiss/toast "hi")`, `(ok "[\"hi\"] from  as user")`},
		{`(call :relay/tell "hi")`, `(ok "[\"hi\"] from relay as user")`},
		{`(call :hiss/fail)`, `(err {:kind :nope})`},
		{kind(`(call :hiss/silent)`), `:timeout`},
		{kind(`(call :hiss/toast)`), `:bad-call`},
		{kind(`(call :hiss/nope)`), `:no-function`},
		{kind(`(call :hiss/review "x")`), `:pending`},
	}
	for _, tt := range tests {
		if got := eval(c, tt.src); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.src, got, tt.want)
		}
	}
	// hiss can call while it is being called, and its calls come from :hiss.
	if got := eval(hiss, `(call :relay/tell "self")`); got != `(ok "[\"self\"] from relay as user")` {
		t.Errorf("hiss calling itself through relay = %s", got)
	}
	historyHas(t, c, `  call  user   :hiss -> :relay/tell "self"  (ok`)

	// The ask waits in :hiss's inbox; its answer goes to hiss's whiff/done.
	id := eval(hiss, `(match (call :hiss/review "y") [(err {:kind :pending :id id}) id])`)
	if text, _ := c.Inbox("hiss"); !strings.Contains(text, id+"  :hiss -> :hiss/review \"y\"") {
		t.Errorf("inbox of :hiss = %q", text)
	}
	if _, err := c.Message("reply", "", "["+id+" (ok 1)]", ""); err != nil {
		t.Fatal(err)
	}
	historyHas(t, c, `  done  user   client -> :hiss/whiff/done `+id+` (ok 1)  (ok :thanks)`)

	describe := eval(c, `(address/describe :hiss)`)
	for _, want := range []string{`{:doc "Shows text." :fn :toast :form :on :mark :read :params [:text]}`,
		`{:doc "Waits for a person." :fn :review :form :ask :mark :write :params [:x]}`} {
		if !strings.Contains(describe, want) {
			t.Errorf("describe :hiss = %s, want it to hold %s", describe, want)
		}
	}
	if ls, _ := c.Ls(); !strings.Contains(ls, ":hiss (client)\n") {
		t.Errorf("ls doesn't mark :hiss as a client's:\n%s", ls)
	}

	// Addresses that are held, or whiff's own, are taken.
	other := dial(t, dir)
	for _, addr := range []string{"hiss", "relay", "jobs", "review", "agent/1"} {
		resp, err := other.Claim(addr, "")
		if err != nil || !strings.HasPrefix(resp.Value, "(err {:kind :taken") {
			t.Errorf("claim :%s = %+v %v, want :taken", addr, resp, err)
		}
	}
	if _, err := hiss.Claim("again", ""); err == nil || err.Error() != "this connection holds :hiss already" {
		t.Errorf("a second claim: %v", err)
	}
	bad := []struct{ src, want string }{
		{`(on (x) "Doc." (ok 1))`, "claim:1:16: a client's on has no body"},
		{`(on (x))`, "claim:1:1: on wants (fn params...) and a doc string"},
		{`(def x 1)`, "claim:1:1: a client's interface is (on ...) and (ask ...) forms, got (def x 1)"},
		{`(on (x) "A.") (on (x) "B.")`, "claim:1:15: x is already defined at claim:1:1"},
	}
	for _, tt := range bad {
		if _, err := other.Claim("other", tt.src); err == nil || err.Error() != tt.want {
			t.Errorf("claim with %s: %v, want %s", tt.src, err, tt.want)
		}
	}

	// A client that hangs up lets go of its address; what waited for it gets
	// :no-address.
	id = eval(c, `(match (call :hiss/review "z") [(err {:kind :pending :id id}) id])`)
	hiss.Close()
	eventually(t, ":hiss let go", func() string { return eval(c, kind(`(call :hiss/toast "x")`)) }, ":no-address")
	historyHas(t, c, `  done  user   :hiss -> client `+id+` (err {:kind :no-address`)
	if resp, err := other.Claim("hiss", `(on (toast text) "Shows text.")`); err != nil || resp.Value != "(ok :done)" {
		t.Errorf("claiming :hiss once it is free = %+v %v", resp, err)
	}
}

func TestAgentClaim(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	claim := func(c *rpc.Client, addr string) string {
		t.Helper()
		resp, err := c.Claim(addr, `(ask (tell text) "Leaves the agent text.")`)
		if err != nil {
			t.Fatalf("claim %s: %v", addr, err)
		}
		return resp.Value
	}
	if got := claim(dial(t, dir), ":agent"); !strings.Contains(got, ":taken") {
		t.Errorf("your claim of :agent = %s, want :taken", got)
	}
	agent := dial(t, dir)
	agent.Agent = true
	if got := claim(agent, ":agent"); got != "(ok :agent/1)" {
		t.Errorf("an agent's claim of :agent = %s, want (ok :agent/1)", got)
	}
	other := dial(t, dir)
	other.Agent = true
	if got := claim(other, ":agent/1"); !strings.Contains(got, ":taken") {
		t.Errorf("an agent's claim of :agent/1 = %s, want :taken", got)
	}
	if got := claim(other, "agent"); got != "(ok :agent/2)" {
		t.Errorf("the second agent's claim = %s, want (ok :agent/2)", got)
	}

	// A message to the agent waits in its inbox, which its evals see.
	resp, err := c.Eval(`(call :agent/1/tell "hi")`, "")
	if err != nil || !strings.Contains(resp.Value, ":pending") {
		t.Fatalf("tell = %+v %v", resp, err)
	}
	resp, err = agent.Eval(`(match (inbox) [[{:fn f :args a}] [f a]])`, "")
	if err != nil || resp.Value != `[:tell ["hi"]]` {
		t.Errorf("the agent's inbox = %+v %v", resp, err)
	}
}
