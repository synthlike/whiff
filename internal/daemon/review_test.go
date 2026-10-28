package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

func TestReviewGate(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(srv notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"asker": `(srv asker
  (on (go) "Asks you." (call :me/ask "add y?"))
  (on (whiff/done id r) "Adds y." (call :notes/add "y")))`,
		"batch": `(srv batch
  (on (twice text) "Adds text twice." (do (try (call :notes/add text)) (call :notes/add text))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		resp, err := c.Message(op, target, args, "")
		if err != nil {
			return "error: " + err.Error()
		}
		return resp.Value
	}
	kind := func(r string) string {
		v, _ := puff.ReadValue(r)
		e, _ := v.(puff.Tag).At(0).(puff.Map)
		k, _ := e.Get(puff.Keyword("kind"))
		return puff.Print(k)
	}
	id := func(r string) string {
		t.Helper()
		v, _ := puff.ReadValue(r)
		n, ok := pendingID(v)
		if !ok {
			t.Fatalf("%s isn't pending", r)
		}
		return puff.Print(puff.Int(n))
	}
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}

	// Nobody reviews yet: an agent's :write message fails; its :read one,
	// and your :write one, go through.
	if r := msg(agent, "call", "notes/add", `["x"]`); kind(r) != ":no-reviewer" {
		t.Errorf("agent's add with no reviewer = %s", r)
	}
	if r := msg(c, "call", "notes/add", `["a"]`); r != "(ok :done)" {
		t.Errorf("your add = %s", r)
	}
	if r := msg(agent, "call", "notes/list", `[]`); r != `(ok "a")` {
		t.Errorf("agent's list = %s", r)
	}

	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending", "review/done"); err != nil {
		t.Fatal(err)
	}
	next := func() rpc.Event {
		t.Helper()
		select {
		case e := <-reviewer.Events():
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return rpc.Event{}
		}
	}

	// An agent's add waits for review, and changes nothing until accepted.
	n := id(msg(agent, "call", "notes/add", `["b"]`))
	if e := next(); e.Topic != "review/pending" || !strings.Contains(e.Value, `:args ["b"]`) ||
		!strings.Contains(e.Value, ":id "+n) || !strings.Contains(e.Value, ":to :notes") || !strings.Contains(e.Value, ":fn :add") {
		t.Errorf("pending event = %+v", e)
	}
	if text, _ := c.Inbox("review"); text != n+"  client -> :notes/add \"b\"\n" {
		t.Errorf("whiff pending = %q", text)
	}
	if got := notes(); got != "a" {
		t.Errorf("notes before the review = %q", got)
	}
	// Only accept or reject answer it, and an agent can't.
	for _, cl := range []*rpc.Client{c, agent} {
		if r := msg(cl, "reply", "", "["+n+" (ok :done)]"); kind(r) != ":bad-call" {
			t.Errorf("reply to a held message = %s", r)
		}
	}
	if r := msg(agent, "accept", "", "["+n+"]"); r != "error: an agent can't review its own messages" {
		t.Errorf("agent's accept = %s", r)
	}

	// Accepting delivers it; its sender gets the result, and so does the
	// reviewer.
	if r := msg(c, "accept", "", "["+n+"]"); r != "(ok :done)" {
		t.Errorf("accept = %s", r)
	}
	if got := notes(); got != "ab" {
		t.Errorf("notes after accepting = %q", got)
	}
	if r := msg(agent, "wait", "", "["+n+"]"); r != "(ok :done)" {
		t.Errorf("agent's wait = %s", r)
	}
	if e := next(); e.Topic != "review/done" || e.Value != "{:id "+n+" :result (ok :done)}" {
		t.Errorf("done event = %+v", e)
	}
	if r := msg(c, "accept", "", "["+n+"]"); kind(r) != ":no-id" {
		t.Errorf("accepting twice = %s", r)
	}

	// What an accepted message leads to goes through too.
	n = id(msg(agent, "call", "batch/twice", `["c"]`))
	next()
	if r := msg(c, "accept", "", "["+n+"]"); r != "(ok :done)" {
		t.Errorf("accept twice = %s", r)
	}
	if got := notes(); got != "abcc" {
		t.Errorf("notes after accepting twice = %q", got)
	}
	next()

	// Rejecting answers it with the reason, and delivers nothing.
	n = id(msg(agent, "send", "notes/add", `["z"]`))
	next()
	if r := msg(c, "reject", "", "["+n+` "not now"]`); r != "(ok :done)" {
		t.Errorf("reject = %s", r)
	}
	r := msg(agent, "wait", "", "["+n+"]")
	if kind(r) != ":rejected" || !strings.Contains(r, `:reason "not now"`) {
		t.Errorf("rejected answer = %s", r)
	}
	if got := notes(); got != "abcc" {
		t.Errorf("notes after rejecting = %q", got)
	}
	if e := next(); e.Topic != "review/done" || !strings.Contains(e.Value, ":rejected") {
		t.Errorf("done event for the reject = %+v", e)
	}

	// Your answer to a message an agent's chain asked is a review too: what
	// it leads to goes through. An agent's answer isn't.
	n = id(msg(agent, "call", "asker/go", `[]`))
	if r := msg(c, "reply", "", "["+n+" (ok :yes)]"); r != "(ok :done)" {
		t.Errorf("your reply = %s", r)
	}
	eventually(t, "y added", func() string {
		if strings.HasSuffix(notes(), "y") {
			return "y added"
		}
		return notes()
	}, "y added")
	n = id(msg(agent, "call", "asker/go", `[]`))
	if r := msg(agent, "reply", "", "["+n+" (ok :yes)]"); r != "(ok :done)" {
		t.Errorf("agent's reply = %s", r)
	}
	if e := next(); e.Topic != "review/pending" || !strings.Contains(e.Value, `:args ["y"]`) || !strings.Contains(e.Value, ":from :asker") {
		t.Errorf("after the agent's reply = %+v", e)
	}
}
