package daemon

import (
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestSub(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"git": `(service git (on (poke v) "Publishes v." (publish :git/changed v)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	some := dial(t, dir)
	all := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	if resp, err := some.Sub("git/changed", ":jobs/done"); err != nil || resp.Value != "(ok :done)" {
		t.Fatalf("sub = %+v %v", resp, err)
	}
	if resp, err := all.Sub(); err != nil || resp.Value != "(ok :done)" {
		t.Fatalf("sub to everything = %+v %v", resp, err)
	}
	next := func(c *rpc.Client) rpc.Event {
		t.Helper()
		select {
		case e := <-c.Events():
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return rpc.Event{}
		}
	}
	same := func(e rpc.Event, topic, value, from, origin string) {
		t.Helper()
		if e.Topic != topic || e.Value != value || e.From != from || e.Origin != origin || e.At.IsZero() {
			t.Errorf("event = %+v, want %s %s from %q as %s", e, topic, value, from, origin)
		}
	}

	c.Eval(`(call :git/poke {:files 2})`, "")
	same(next(some), "git/changed", "{:files 2}", "git", "user")
	same(next(all), "git/changed", "{:files 2}", "git", "user")

	// A topic some didn't ask for reaches only all; an agent's publish is
	// the agent's.
	if _, err := agent.Message("pub", "other/thing", `["x"]`, ""); err != nil {
		t.Fatal(err)
	}
	same(next(all), "other/thing", `"x"`, "", "agent")
	c.Eval(`(call :git/poke 3)`, "")
	same(next(some), "git/changed", "3", "git", "user")
	same(next(all), "git/changed", "3", "git", "user")

	// A client that doesn't read its events doesn't hold up publishers.
	done := make(chan struct{})
	go func() {
		c.Eval(`(vector/each (vector/range 0 500) (fn (i) (publish :flood/x i)))`, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing to a client that doesn't read blocked")
	}

	for _, tt := range []struct {
		c    *rpc.Client
		args []string
		want string
	}{
		{some, nil, "this connection is subscribed already"},
		{c, []string{"bad topic"}, "want topics as a vector of keywords, got [:bad topic]"},
	} {
		if _, err := tt.c.Sub(tt.args...); err == nil || err.Error() != tt.want {
			t.Errorf("sub %q: %v, want %s", tt.args, err, tt.want)
		}
	}

	// Hanging up ends the subscription and closes the events channel;
	// publishing goes on.
	some.Close()
	for range some.Events() {
	}
	if resp, err := c.Eval(`(call :git/poke 4)`, ""); err != nil || resp.Value != "(ok :done)" {
		t.Errorf("publishing after a subscriber hung up = %+v %v", resp, err)
	}
}
