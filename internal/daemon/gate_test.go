package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// TestGateTable is the review gate as one table: who sends, to what, and
// whether it goes through, is held for review or refused. Each row's tag
// is in its message's arguments; the outcome is read from the history
// entry that first carries it, as the gate wrote it. A path that forgets
// a rule shows up as one row, as a session that left out the agent flag
// did. A peer's rows are TestFar's: an agent's write held there, and a
// task an agent answers there coming back as an agent's.
func TestGateTable(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"box": `(service box
  (on (r x) :read "Reads." (ok x))
  (on (w x) :write "Writes." (ok x))
  (ask (q x) "A question."))`,
		// relay's functions are what they reach: w is :write by inference.
		"relay": `(service relay
  (on (r x) "Reads box." (call :box/r x))
  (on (w x) "Writes box." (call :box/w x)))`,
		// asker writes box with a job's output once it ends: a model's,
		// :origin :agent, is the agent's; a plain one's is yours.
		"asker": `(service asker
  (on (model x) "A model's job." (call :jobs/run ["echo" x] {:origin :agent :as :text}))
  (on (plain x) "A plain job." (call :jobs/run ["echo" x] {:as :text}))
  (on (whiff/done id r) "Writes box." (call :box/w (value/to-string r))))`,
	})
	start(t, dir)
	you := dial(t, dir)
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	// agent is a new agent connection holding :agent/<n>.
	agent := func() (*rpc.Client, string) {
		c := dial(t, dir)
		c.Agent = true
		resp, err := c.Claim("agent", "")
		if err != nil {
			t.Fatal(err)
		}
		return c, strings.TrimSuffix(strings.TrimPrefix(resp.Value, "(ok :"), ")")
	}
	call := func(c *rpc.Client, target, tag string) {
		t.Helper()
		if _, err := c.Message("call", target, `["`+tag+`"]`, dir); err != nil {
			t.Fatal(err)
		}
	}
	eval := func(c *rpc.Client, src string) {
		t.Helper()
		if _, err := c.Eval(src, dir); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		who, what string
		send      func(tag string)
		want      string // through, held or refused
	}{
		{"you", "a :write", func(tag string) { call(you, "box/w", tag) }, "through"},
		{"an agent", "a :read", func(tag string) { a, _ := agent(); call(a, "box/r", tag) }, "through"},
		{"an agent", "a :write", func(tag string) { a, _ := agent(); call(a, "box/w", tag) }, "held"},
		{"an agent", "an ask", func(tag string) { a, _ := agent(); call(a, "box/q", tag) }, "through"},
		{"a session leaving the flag out", "a :write", func(tag string) {
			a, _ := agent()
			a.Agent = false
			call(a, "box/w", tag)
		}, "held"},
		{"a session trusted for box", "a :write", func(tag string) {
			a, name := agent()
			you.Mode(name, "trusted", []string{"box"}, 0)
			call(a, "box/w", tag)
		}, "through"},
		{"a session trusted for relay", "box's :write", func(tag string) {
			a, name := agent()
			you.Mode(name, "trusted", []string{"relay"}, 0)
			call(a, "box/w", tag)
		}, "held"},
		{"a free session", "a :write", func(tag string) {
			a, name := agent()
			you.Mode(name, "free", nil, 0)
			call(a, "box/w", tag)
		}, "through"},
		{"an agent", "a service that writes", func(tag string) { a, _ := agent(); call(a, "relay/w", tag) }, "held"},
		{"an agent", "a service that reads", func(tag string) { a, _ := agent(); call(a, "relay/r", tag) }, "through"},
		{"your chain", "a :write further down", func(tag string) { call(you, "relay/w", tag) }, "through"},
		{"a model's job's result", "a :write", func(tag string) { call(you, "asker/model", tag) }, "held"},
		{"a plain job's result", "a :write", func(tag string) { call(you, "asker/plain", tag) }, "through"},
		{"an agent's eval", "a :write", func(tag string) { a, _ := agent(); eval(a, `(call :box/w "`+tag+`")`) }, "held"},
		{"an agent's eval", "a :read", func(tag string) { a, _ := agent(); eval(a, `(call :box/r "`+tag+`")`) }, "through"},
	}
	for i, tt := range tests {
		tag := "row" + string(rune('a'+i))
		tt.send(tag)
		got := ""
		for deadline := time.Now().Add(3 * time.Second); got == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			got = outcome(t, you, tag)
		}
		if got != tt.want {
			h, _ := historyOf(you)
			t.Errorf("%s, %s: %s, want %s\n%s", tt.who, tt.what, got, tt.want, h)
		}
	}
}

// outcome is how the gate treated the first message to box whose
// arguments carry tag: held, refused or through; "" if none has come.
func outcome(t *testing.T, c *rpc.Client, tag string) string {
	t.Helper()
	es, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if !strings.Contains(e.Args, tag) || !strings.HasPrefix(e.To, "box/") && !strings.HasPrefix(e.To, "relay/") && e.Kind != "eval" {
			continue
		}
		switch {
		case strings.HasPrefix(e.Gate, "held"):
			return "held"
		case e.Gate == "refused":
			return "refused"
		case e.Kind == "eval" || strings.HasPrefix(e.To, "box/"):
			return "through"
		}
	}
	return ""
}
