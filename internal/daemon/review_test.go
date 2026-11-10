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
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"asker": `(service asker
  (on (go) "Asks you." (call :me/ask "add y?"))
  (on (agent) "Asks the agent." (call :agent/1/tell "add y?"))
  (on (whiff/done id r) "Adds y." (call :notes/add "y")))`,
		"batch": `(service batch
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
		k := puff.Field(e, "kind")
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
	// A job is :read when its command is, so only the job's own pending.
	if r := msg(agent, "call", "jobs/run", `[["pwd"]]`); kind(r) != ":pending" {
		t.Errorf("agent's read-only job = %s", r)
	}
	if r := msg(agent, "call", "jobs/run", `[["touch" "`+file+`"]]`); kind(r) != ":no-reviewer" {
		t.Errorf("agent's job with no reviewer = %s", r)
	}
	// An agent following :review/pending isn't a reviewer: it can't accept.
	watching := dial(t, dir)
	watching.Agent = true
	if _, err := watching.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	if r := msg(agent, "call", "notes/add", `["x"]`); kind(r) != ":no-reviewer" {
		t.Errorf("agent's add with only an agent following = %s", r)
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
	if r := msg(c, "reply", "", "["+n+" (ok :done)]"); kind(r) != ":bad-call" {
		t.Errorf("reply to a held message = %s", r)
	}
	if r := msg(agent, "reply", "", "["+n+" (ok :done)]"); !strings.Contains(r, "only its own inbox") {
		t.Errorf("agent's reply to a held message = %s", r)
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
	// An agent can't answer the question to you; its answer in its own
	// inbox goes straight through, and what it leads to waits.
	n = id(msg(agent, "call", "asker/go", `[]`))
	if r := msg(agent, "reply", "", "["+n+" (ok :yes)]"); !strings.Contains(r, "only its own inbox") {
		t.Errorf("agent's reply to your question = %s", r)
	}
	if resp, err := agent.Claim("agent", `(ask (tell text) "Asks the agent.")`); err != nil || resp.Value != "(ok :agent/1)" {
		t.Fatalf("claim = %+v %v", resp, err)
	}
	n = id(msg(c, "call", "asker/agent", `[]`))
	if r := msg(agent, "reply", "", "["+n+" (ok :yes)]"); r != "(ok :done)" {
		t.Errorf("agent's reply in its inbox = %s", r)
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
		"notes": `(service notes
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
		`(proc/output ["pwd"] {:dir "/"})`:       `(ok "/\n")`,
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
		`(proc/output ["touch" "` + file + `"])`,
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
		"notes": `(service notes
  (on :t/write (v) (file/append "` + file + `" v))
  (on :t/read (v) (ok v)))`,
		"relay": `(service relay
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

// :review's functions review as whiff pending, accept and reject do, for
// your messages only: not an agent's, nor one in a chain it began that you
// accepted.
func TestReviewService(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text)))`,
		"relay": `(service relay
  (on (pass id) "Accepts id." (call :review/accept id)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		resp, err := c.Message(op, target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	call := func(c *rpc.Client, target, args string) string {
		t.Helper()
		return msg(c, "call", target, args)
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
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}

	n := held(call(agent, "notes/add", `["a"]`))
	m := held(call(agent, "notes/add", `["b"]`))
	if r := call(c, "review/pending", `[]`); !strings.HasPrefix(r, `(ok (items [{:actions [{:action [:review/accept `+n+`] :label "Accept"} {:action [:review/reject `+n+`] :ask "Why? Its sender reads this." :label "Reject"}] :detail "`+n+` from client in " :label ":notes/add \"a\""}`) ||
		!strings.HasSuffix(r, `] {:changed [:review/pending :review/done]}))`) {
		t.Errorf("review/pending = %s", r)
	}

	// An agent can't review: from its connection, where its message to
	// :review isn't held either, as that would protect nothing; nor by a
	// service you accept for it, whether you accept with whiff accept or
	// :review/accept, which a call back to :review can't wait for.
	for _, tt := range [][2]string{{"review/pending", `[]`}, {"review/accept", "[" + n + "]"}, {"review/reject", "[" + n + "]"}} {
		if r := call(agent, tt[0], tt[1]); !strings.Contains(r, ":kind :refused") {
			t.Errorf("agent's %s = %s", tt[0], r)
		}
	}
	p := held(call(agent, "relay/pass", "["+n+"]"))
	if r := msg(c, "accept", "", "["+p+"]"); !strings.Contains(r, ":kind :refused") {
		t.Errorf("relay/pass accepted with whiff accept = %s", r)
	}
	p = held(call(agent, "relay/pass", "["+n+"]"))
	if r := call(c, "review/accept", "["+p+"]"); !strings.Contains(r, ":kind :cycle") {
		t.Errorf("relay/pass accepted with :review/accept = %s", r)
	}
	if got := notes(); got != "" {
		t.Errorf("notes after the agent's tries = %q", got)
	}

	// Yours accept and reject.
	if r := call(c, "review/accept", "["+n+"]"); r != "(ok :done)" {
		t.Errorf("review/accept = %s", r)
	}
	if got := notes(); got != "a" {
		t.Errorf("notes after accept = %q", got)
	}
	if r := call(c, "review/reject", "["+m+"]"); r != "(ok :done)" {
		t.Errorf("review/reject = %s", r)
	}
	if r := call(c, "review/accept", "["+m+"]"); !strings.Contains(r, ":kind :no-id") {
		t.Errorf("accept of a rejected one = %s", r)
	}

	// Trust accepts an agent session's message, and lets its next ones to
	// that service through. A message that isn't a session's has no Trust.
	sess := dial(t, dir)
	sess.Agent = true
	if resp, err := sess.Claim("agent", ""); err != nil || resp.Value != "(ok :agent/1)" {
		t.Fatalf("claim = %+v %v", resp, err)
	}
	q := held(call(sess, "notes/add", `["c"]`))
	o := held(call(agent, "notes/add", `["x"]`))
	if r := call(c, "review/pending", `[]`); !strings.Contains(r, `{:action [:review/trust `+q+`] :label "Trust :agent/1 for :notes"}`) ||
		strings.Contains(r, "[:review/trust "+o+"]") {
		t.Errorf("review/pending with a session's = %s", r)
	}
	if r := call(sess, "review/trust", "["+q+"]"); !strings.Contains(r, ":kind :refused") {
		t.Errorf("the session's own trust = %s", r)
	}
	if r := call(c, "review/trust", "["+o+"]"); !strings.Contains(r, ":kind :bad-call") {
		t.Errorf("trust of a client's = %s", r)
	}
	if r := call(c, "review/trust", "["+q+"]"); r != "(ok :done)" {
		t.Errorf("review/trust = %s", r)
	}
	if r := call(sess, "notes/add", `["d"]`); r != "(ok :done)" {
		t.Errorf("the session's next add = %s", r)
	}
	if got := notes(); got != "acd" {
		t.Errorf("notes after trust = %q", got)
	}
	if r := call(c, "review/reject", "["+o+"]"); r != "(ok :done)" {
		t.Errorf("review/reject = %s", r)
	}
	if r := call(c, "review/pending", `[]`); r != `(ok (items [] {:changed [:review/pending :review/done]}))` {
		t.Errorf("review/pending once empty = %s", r)
	}
}

// TestAcceptApart: while :review/accept waits on what it delivered, a slow
// service here, :review answers its other calls, and accepts another.
func TestAcceptApart(t *testing.T) {
	dir := tempDir(t)
	gate := filepath.Join(dir, "go")
	writeServices(t, dir, map[string]string{
		"slow": `(service slow
  (on (wait) "Waits for the go file." (proc/output ["sh" "-c" "while [ ! -e ` + gate + ` ]; do sleep 0.02; done; echo went"])))`,
	})
	start(t, dir)
	c, other := dial(t, dir), dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	pend := func() string {
		t.Helper()
		resp, err := agent.Message("call", "slow/wait", `[]`, "")
		if err != nil {
			t.Fatal(err)
		}
		v, _ := puff.ReadValue(resp.Value)
		n, ok := pendingID(v)
		if !ok {
			t.Fatalf("%s isn't pending", resp.Value)
		}
		return puff.Print(puff.Int(n))
	}
	first, second := pend(), pend()
	accepted := make(chan string, 1)
	go func() {
		resp, err := c.Message("call", "review/accept", "["+first+"]", "")
		if err != nil {
			accepted <- err.Error()
			return
		}
		accepted <- resp.Value
	}()
	// The first accept waits on :slow; :review still answers.
	eventually(t, "the first accept waiting", func() string {
		resp, _ := other.Message("call", "review/pending", `[]`, "")
		if strings.Contains(resp.Value, "[:review/accept "+first+"]") {
			return resp.Value
		}
		return "waiting"
	}, "waiting")
	done := make(chan string, 1)
	go func() {
		resp, _ := other.Message("call", "review/reject", "["+second+` "no"]`, "")
		done <- resp.Value
	}()
	select {
	case r := <-done:
		if r != "(ok :done)" {
			t.Errorf("reject while an accept waits = %s", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reject waited behind the accept")
	}
	select {
	case r := <-accepted:
		t.Fatalf("the accept ended before :slow went on: %s", r)
	default:
	}
	os.WriteFile(gate, nil, 0o600)
	if r := <-accepted; r != `(ok "went\n")` {
		t.Errorf("accept = %s", r)
	}
}
