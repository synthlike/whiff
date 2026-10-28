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

func TestEvalGate(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	os.WriteFile(file, []byte("a"), 0o600)
	writeServices(t, dir, map[string]string{
		"notes": `(srv notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	eval := func(c *rpc.Client, src string) string {
		t.Helper()
		resp, err := c.Eval(src, "")
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if resp.Error != "" {
			return "error: " + resp.Error
		}
		return resp.Value
	}
	held := func(src string) string {
		t.Helper()
		r := eval(agent, src)
		v, _ := puff.ReadValue(r)
		n, ok := pendingID(v)
		if !ok {
			t.Fatalf("%s = %s, want it held", src, r)
		}
		return puff.Print(puff.Int(n))
	}
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}
	write := `(file/write "` + file + `" "x")`

	if r := eval(agent, write); !strings.Contains(r, ":no-reviewer") {
		t.Errorf("agent's write with no reviewer = %s", r)
	}
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}

	// Code that only reads runs; so does yours that writes.
	for src, want := range map[string]string{
		`(file/read-text "` + file + `")`:        `(ok "a")`,
		`(call :notes/list)`:                     `(ok "a")`,
		`(def (peek) (call :notes/list)) (peek)`: `(ok "a")`,
	} {
		if r := eval(agent, src); r != want {
			t.Errorf("agent's %s = %s, want %s", src, r, want)
		}
	}
	if r := eval(c, `(file/append "`+file+`" "b")`); r != "(ok :done)" {
		t.Errorf("your append = %s", r)
	}

	// Code that reaches :write waits as a whole, however it gets there.
	for _, src := range []string{
		`(call :notes/add "z")`,
		`(def (w) ` + write + `) (w)`,
		`(vector/map ["z"] (fn (s) (call :notes/add s)))`,
	} {
		n := held(src)
		if r, err := c.Message("reject", "", "["+n+` "no"]`, ""); err != nil || r.Value != "(ok :done)" {
			t.Errorf("reject %s = %+v %v", src, r, err)
		}
	}
	if got := notes(); got != "ab" {
		t.Errorf("notes after holding = %q", got)
	}

	// Accepting runs it, and its sender gets the result.
	n := held(write)
	if text, _ := c.Inbox("review"); text != n+`  client -> eval "(file/write \"`+file+`\" \"x\")"`+"\n" {
		t.Errorf("whiff pending = %q", text)
	}
	if r, err := c.Message("accept", "", "["+n+"]", ""); err != nil || r.Value != "(ok :done)" {
		t.Errorf("accept = %+v %v", r, err)
	}
	if got := notes(); got != "x" {
		t.Errorf("notes after accepting = %q", got)
	}
	if r, err := agent.Message("wait", "", "["+n+"]", ""); err != nil || r.Value != "(ok :done)" {
		t.Errorf("agent's wait = %+v %v", r, err)
	}

	// A held repl form is gated the same way.
	resp, err := agent.Repl(`(file/append "`+file+`" "y")`, "")
	if err != nil || !strings.Contains(resp.Value, ":pending") {
		t.Errorf("agent's repl write = %+v %v", resp, err)
	}
}

func TestPublishGate(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(srv notes
  (on :t/write (v) (file/append "` + file + `" v))
  (on :t/read (v) (ok v)))`,
		"relay": `(srv relay
  (on (shout v) :read "Publishes v; claims :read, so its call isn't held." (publish :t/write v)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}
	added := func(want string) {
		t.Helper()
		eventually(t, want, notes, want)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		resp, err := c.Message(op, target, args, "")
		if err != nil {
			t.Fatalf("%s %s: %v", op, target, err)
		}
		return resp.Value
	}
	held := func(r string) string {
		t.Helper()
		v, _ := puff.ReadValue(r)
		n, ok := pendingID(v)
		if !ok {
			t.Fatalf("%s isn't pending", r)
		}
		return puff.Print(puff.Int(n))
	}

	// To :read subscribers, and yours, go straight through.
	if r := msg(agent, "pub", "t/read", `[1]`); r != "(ok :done)" {
		t.Errorf("agent's pub to :read subscribers = %s", r)
	}
	if r := msg(c, "pub", "t/write", `["a"]`); r != "(ok :done)" {
		t.Errorf("your pub = %s", r)
	}
	added("a")

	// An agent's, from a client or a service, waits for review.
	n := held(msg(agent, "pub", "t/write", `["b"]`))
	if text, _ := c.Inbox("review"); text != n+`  client -> publish :t/write "b"`+"\n" {
		t.Errorf("whiff pending = %q", text)
	}
	m := held(msg(agent, "call", "relay/shout", `["c"]`))
	if got := notes(); got != "a" {
		t.Errorf("notes before the review = %q", got)
	}
	if r := msg(c, "accept", "", "["+n+"]"); r != "(ok :done)" {
		t.Errorf("accept = %s", r)
	}
	added("ab")
	if r := msg(c, "accept", "", "["+m+"]"); r != "(ok :done)" {
		t.Errorf("accept the service's = %s", r)
	}
	added("abc")
}
