package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// stopDaemon stops the daemon c is connected to, whose start gave done.
func stopDaemon(t *testing.T, c *rpc.Client, done <-chan error) {
	t.Helper()
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// pendingOf is the id in r, a printed pending.
func pendingOf(t *testing.T, r string) string {
	t.Helper()
	v, err := puff.ReadValue(r)
	if err != nil {
		t.Fatalf("%s: %v", r, err)
	}
	n, ok := pendingID(v)
	if !ok {
		t.Fatalf("%s isn't pending", r)
	}
	return strconv.FormatInt(n, 10)
}

// What waits in a service's inbox outlives the daemon, and its answer
// reaches the sender; an ask's :expires goes on counting.
func TestInboxesSurviveRestart(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	box := `(service box
  (ask (q x) "A question.")
  (ask (hour x) "Ends while whiff is down." :expires 1h)
  (ask (later x) "Ends after whiff is back." :expires 1s))`
	writeServices(t, dir, map[string]string{
		"box": box,
		"asker": `(service asker
  (on (put which x) "Asks box." (match which
    [:q (call :box/q x)]
    [:hour (call :box/hour x)]
    [:later (call :box/later x)]))
  (on (whiff/done id r) "Notes the answer." (file/append "` + got + `" (string/concat (value/to-string r) "\n"))))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	send := func(fn, x string) string {
		t.Helper()
		r, err := c.Message("call", "asker/put", `[:`+fn+` "`+x+`"]`, "")
		if err != nil {
			t.Fatal(err)
		}
		return pendingOf(t, r.Value)
	}
	answers := func() string {
		b, _ := os.ReadFile(got)
		return string(b)
	}
	q, hour := send("q", "one"), send("hour", "two")
	later := send("later", "three")
	stopDaemon(t, c, done)

	// The ask came two hours ago, as far as its stored time goes.
	db, err := bbolt.Open(filepath.Join(dir, "whiff.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.ParseInt(hour, 10, 64)
	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(inboxBucket)
		a, err := storedAsked(n, string(b.Get(idKey(n))))
		if err != nil {
			return err
		}
		a.at = a.at.Add(-2 * time.Hour)
		return b.Put(idKey(n), []byte(puff.Print(a.stored())))
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	done = start(t, dir)
	c = dial(t, dir)
	// It passed while whiff was down, and the answer finds its sender.
	first := `(err {:kind :expired :message ":box/hour waited longer than 1h0m0s"})` + "\n"
	eventually(t, "the ask that expired while whiff was down", answers, first)
	if text, _ := c.Inbox("box"); !strings.Contains(text, q+"  :asker -> :box/q") || strings.Contains(text, hour+"  ") {
		t.Errorf("inbox of :box = %q", text)
	}
	eventually(t, "the ask that expired after", answers, first+`(err {:kind :expired :message ":box/later waited longer than 1s"})`+"\n")
	if text, _ := c.Inbox("box"); strings.Contains(text, later+"  ") {
		t.Errorf("expired, it is still in the inbox: %q", text)
	}

	// A service that isn't loaded keeps its messages for when it is.
	stopDaemon(t, c, done)
	if err := os.Remove(filepath.Join(dir, "services", "box.puff")); err != nil {
		t.Fatal(err)
	}
	done = start(t, dir)
	c = dial(t, dir)
	historyHas(t, c, "1 waiting for :box, which isn't loaded")
	if text, _ := c.Inbox("box"); !strings.Contains(text, q+"  ") {
		t.Errorf("inbox of an address that isn't loaded = %q", text)
	}
	stopDaemon(t, c, done)
	writeServices(t, dir, map[string]string{"box": box})
	start(t, dir)
	c = dial(t, dir)
	before := answers()
	if _, err := c.Message("reply", "", "["+q+` (ok "yes")]`, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "your answer", answers, before+`(ok "yes")`+"\n")
}

// What the review gate holds outlives the daemon, as it was sent: accepting
// delivers it, and rejecting tells its sender. A session's number stays its
// own, so Trust can't reach another's.
func TestReviewSurvivesRestart(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	evald := filepath.Join(dir, "evald")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on :t/write (v) (file/append "` + file + `" v)))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	agent := dial(t, dir)
	agent.Agent = true
	if resp, err := agent.Claim("agent", ""); err != nil || resp.Value != "(ok :agent/1)" {
		t.Fatalf("claim: %+v %v", resp, err)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		r, err := c.Message(op, target, args, dir)
		if err != nil {
			t.Fatalf("%s %s: %v", op, target, err)
		}
		return r.Value
	}
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}
	accepted := pendingOf(t, msg(agent, "call", "notes/add", `["accepted \"quoted\""]`))
	rejected := pendingOf(t, msg(agent, "call", "notes/add", `["rejected"]`))
	trusted := pendingOf(t, msg(agent, "call", "notes/add", `["trusted"]`))
	published := pendingOf(t, msg(agent, "pub", "t/write", `[" published"]`))
	resp, err := agent.Eval(`(file/write "`+evald+`" "ran")`, dir)
	if err != nil {
		t.Fatal(err)
	}
	evaled := pendingOf(t, resp.Value)
	queue, err := c.Inbox("review")
	if err != nil || strings.Count(queue, "\n") != 5 {
		t.Fatalf("queue = %q %v", queue, err)
	}

	stopDaemon(t, c, done)
	start(t, dir)
	c = dial(t, dir)
	if text, err := c.Inbox("review"); err != nil || text != queue {
		t.Errorf("queue after a restart = %q %v, want %q", text, err, queue)
	}
	if n := notes(); n != "" {
		t.Errorf("something held ran: %q", n)
	}

	// A new session gets a new number, and Trust doesn't reach it.
	agent = dial(t, dir)
	agent.Agent = true
	if resp, err := agent.Claim("agent", ""); err != nil || resp.Value != "(ok :agent/2)" {
		t.Fatalf("a new session: %+v %v", resp, err)
	}
	if r := msg(c, "call", "review/pending", "[]"); strings.Contains(r, ":review/trust") || !strings.Contains(r, ":review/accept") {
		t.Errorf("a session that has gone is offered Trust: %s", r)
	}
	if r := msg(c, "call", "review/trust", "["+trusted+"]"); !strings.Contains(r, ":no-address") {
		t.Errorf("trusting a session that has gone = %s", r)
	}
	if text, _ := c.Inbox("review"); !strings.Contains(text, trusted+"  ") {
		t.Errorf("a failed Trust took the message: %q", text)
	}

	if r := msg(c, "accept", "", "["+accepted+"]"); r != "(ok :done)" {
		t.Errorf("accept = %s", r)
	}
	if n := notes(); n != `accepted "quoted"` {
		t.Errorf("notes = %q", n)
	}
	if r := msg(c, "accept", "", "["+published+"]"); r != "(ok :done)" {
		t.Errorf("accept a publish = %s", r)
	}
	eventually(t, "the publish", notes, `accepted "quoted" published`)
	if r := msg(c, "accept", "", "["+evaled+"]"); r != "(ok :done)" {
		t.Errorf("accept an eval = %s", r)
	}
	if b, _ := os.ReadFile(evald); string(b) != "ran" {
		t.Errorf("the eval wrote %q", b)
	}
	if r := msg(c, "reject", "", "["+rejected+` "no"]`); r != "(ok :done)" {
		t.Errorf("reject = %s", r)
	}
	if r := msg(c, "wait", "", "["+rejected+"]"); !strings.Contains(r, ":rejected") {
		t.Errorf("wait for the rejected message = %s", r)
	}
	if r := msg(c, "reject", "", "["+trusted+` ""]`); r != "(ok :done)" {
		t.Errorf("reject = %s", r)
	}
	if text, _ := c.Inbox("review"); text != "" {
		t.Errorf("queue = %q, want it empty", text)
	}
}

func TestStoredAsked(t *testing.T) {
	const at = `:cwd "/x" :at 2026-10-27T10:00:00Z`
	a, err := storedAsked(1, `{:addr :me :fn :ask :args ["q"] :origin :user :from :plan :session :agent/3 `+at+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if a.id != 1 || a.addr != "me" || a.fn != "ask" || a.from != "plan" || a.dir != "/x" || !a.user || a.session != "agent/3" || a.at.Year() != 2026 {
		t.Errorf("storedAsked = %+v", a)
	}
	if a.msg != 0 {
		t.Errorf("message ids begin again with a daemon: %+v", a)
	}
	// Each comes back the way keep stored it.
	for _, a := range []*asked{
		{ticket: ticket{id: 2, from: "agent/1", dir: "/x", session: "agent/1"}, addr: "review", fn: "add", args: puff.NewVector(puff.String("x")), at: time.Unix(1, 0), to: "notes",
			held: &message{from: "agent/1", fn: "add", args: `["x"]`, dir: "/x", session: "agent/1"}},
		{ticket: ticket{id: 3, dir: "/x"}, addr: "review", fn: "publish", args: puff.NewVector(puff.Int(1)), at: time.Unix(1, 0), to: "t/write",
			held: &message{topic: "t/write", args: "[1]", dir: "/x"}},
		{ticket: ticket{id: 4, dir: "/x"}, addr: "review", fn: "eval", args: puff.NewVector(puff.String("(f)")), at: time.Unix(1, 0), src: "(f)"},
		{ticket: ticket{id: 5, user: true, dir: "/x"}, addr: "box", fn: "q", args: puff.NewVector(), at: time.Unix(1, 0), expires: time.Hour},
	} {
		b, err := storedAsked(a.id, puff.Print(a.stored()))
		if err != nil {
			t.Fatalf("%+v: %v", a, err)
		}
		if p, q := puff.Print(a.stored()), puff.Print(b.stored()); p != q {
			t.Errorf("stored %s, read back %s", p, q)
		}
		if a.what() != b.what() && a.addr == "review" {
			t.Errorf("held %s, read back %s", a.what(), b.what())
		}
	}
	// Only a stored :user is yours.
	for _, p := range []string{
		`{:addr :me :fn :ask :args ["q"] ` + at + `}`,
		`{:addr :me :fn :ask :args ["q"] :origin "user" ` + at + `}`,
	} {
		if a, err := storedAsked(1, p); err != nil || a.user {
			t.Errorf("storedAsked(%s) = %+v %v, want an agent's", p, a, err)
		}
	}
	for _, p := range []string{
		`[1]`,
		`{:fn :q :args []}`,
		`{:addr :box :args []}`,
		`{:addr :box :fn :q}`,
		`{:addr :me :fn :answer :args ["q"]}`,
		`{:addr :me :fn :ask :args []}`,
		`{:addr :me :fn :ask :args ["q" 1 2]}`,
		`{:addr :me :fn :ask :args ["q" 1]}`,
		`{:addr :me :fn :tell :args ["q" {:actions 1}]}`,
		`{:addr :review :fn :add :args []}`,
		`{:addr :review :fn :add :args [] :held :nope :to :notes}`,
		`{:addr :review :fn :add :args [] :held :message}`,
		`{:addr :review :fn :eval :args [1] :held :eval}`,
	} {
		if a, err := storedAsked(1, p); err == nil {
			t.Errorf("storedAsked(%s) = %+v, want an error", p, a)
		}
	}
}

func TestTrialsAreNotStored(t *testing.T) {
	if (&asked{addr: "try/1"}).durable() {
		t.Error("a trial's inbox is durable")
	}
	if !(&asked{addr: "box"}).durable() {
		t.Error("a service's inbox isn't durable")
	}
}
