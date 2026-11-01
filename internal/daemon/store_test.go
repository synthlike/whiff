package daemon

import (
	"os"
	"path/filepath"
	"slices"
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

// storedTimers is the timers stored for addr, in the whiff.db in dir.
func storedTimers(t *testing.T, dir, addr string) int {
	t.Helper()
	n := 0
	withDB(t, dir, func(db *bbolt.DB) {
		db.View(func(tx *bbolt.Tx) error {
			return tx.Bucket(timersBucket).ForEach(func(k, v []byte) error {
				if tm, err := storedTimer(k, string(v)); err == nil && tm.addr == addr {
					n++
				}
				return nil
			})
		})
	})
	return n
}

// withDB gives f the whiff.db in dir, whose daemon has stopped.
func withDB(t *testing.T, dir string, f func(db *bbolt.DB)) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, "whiff.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f(db)
}

// makeDue makes the timers stored for fn due now, as if they came due while
// whiff was down.
func makeDue(t *testing.T, dir, fn string) {
	t.Helper()
	withDB(t, dir, func(db *bbolt.DB) {
		err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(timersBucket)
			due := map[string]*timer{}
			err := b.ForEach(func(k, v []byte) error {
				tm, err := storedTimer(slices.Clone(k), string(v))
				if err != nil {
					return err
				}
				if tm.m.fn == fn {
					due[string(k)] = tm
				}
				return nil
			})
			if err != nil {
				return err
			}
			for k, tm := range due {
				if err := b.Delete([]byte(k)); err != nil {
					return err
				}
				tm.due = time.Now().Add(-time.Hour)
				if err := b.Put(tm.key, []byte(puff.Print(tm.stored()))); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// A send-after outlives the daemon: one that came due while whiff was down
// fires once, and one that didn't waits. It keeps its origin, so an agent's
// still meets the review gate. A :start sets its own, so a loop isn't doubled.
func TestTimersSurviveRestart(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text)))`,
		"relay": `(service relay
  (on (later text) :read "Adds text, later." (send-after 1h :notes/add text)))`,
		"remind": `(service remind
  (on (me text) "Sets a reminder to itself." (send-after 1h :ring text))
  (on (ring text) "Rings." (call :notes/add text)))`,
		"ticker": `(service ticker :start (fn () (send-after 1h :tick))
  (on (tick) "Ticks, and ticks again." (send-after 1h :tick)))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	notes := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}
	eval := func(cl *rpc.Client, src string) {
		t.Helper()
		if r, err := cl.Eval(src, dir); err != nil || r.Error != "" || r.Value != "(ok :done)" {
			t.Fatalf("%s: %+v %v", src, r, err)
		}
	}
	eval(c, `(call :remind/me "rung ")`)
	eval(c, `(send-after 1h :notes/add "due ")`)
	eval(agent, `(call :relay/later "an agent's ")`)
	// A trial's timers go with it: nothing of this is left to come back.
	if r, err := c.Try("trial.puff", `(service trial (on (go) "Sets a timer." (send-after 1h :go)))`, "go", "[]", dir); err != nil || r.Error != "" {
		t.Fatalf("try: %+v %v", r, err)
	}
	stopDaemon(t, c, done)
	if n := storedTimers(t, dir, "ticker"); n != 1 {
		t.Fatalf("%d timers stored for :ticker, want 1", n)
	}
	withDB(t, dir, func(db *bbolt.DB) {
		db.View(func(tx *bbolt.Tx) error {
			if n := tx.Bucket(timersBucket).Stats().KeyN; n != 4 {
				t.Errorf("%d timers stored, want 4, none of them the trial's", n)
			}
			return nil
		})
	})
	makeDue(t, dir, "add")
	makeDue(t, dir, "ring")

	done = start(t, dir)
	c = dial(t, dir)
	historyHas(t, c, ":ticker's :start set its timers again, so whiff dropped 1 stored")
	// Whichever comes first: the agent's isn't among them.
	eventually(t, "the timers that came due", func() string {
		n := notes()
		if strings.Contains(n, "rung ") && strings.Contains(n, "due ") {
			return "both"
		}
		return n
	}, "both")
	historyHas(t, c, `notes/add "an agent's "`)
	if n := notes(); strings.Contains(n, "an agent's") {
		t.Errorf("an agent's timer passed the review gate: %q", n)
	}
	stopDaemon(t, c, done)
	if n := storedTimers(t, dir, "ticker"); n != 1 {
		t.Errorf("%d timers stored for :ticker, want 1: the loop doubled", n)
	}
	if n := storedTimers(t, dir, "notes") + storedTimers(t, dir, "remind"); n != 0 {
		t.Errorf("%d timers that fired are still stored", n)
	}

	// None of them fires again.
	before := notes()
	start(t, dir)
	c = dial(t, dir)
	time.Sleep(50 * time.Millisecond)
	if n := notes(); n != before {
		t.Errorf("notes = %q, want %q", n, before)
	}
}

func TestStoredTimer(t *testing.T) {
	tm := &timer{key: []byte("k"), due: time.Unix(100, 0), addr: "notes", m: message{from: "agent/1", fn: "add", args: `["x" 1]`, dir: "/x", reviewed: true, session: "agent/1"}}
	got, err := storedTimer(tm.key, puff.Print(tm.stored()))
	if err != nil {
		t.Fatal(err)
	}
	if !got.due.Equal(tm.due) || got.addr != "notes" || got.m.from != "agent/1" || got.m.fn != "add" || got.m.args != `["x" 1]` ||
		got.m.dir != "/x" || got.m.user || !got.m.reviewed || got.m.session != "agent/1" {
		t.Errorf("storedTimer = %+v", got)
	}
	// Only what was stored as yours, or as reviewed, is.
	got, err = storedTimer(nil, `{:due 2026-10-27T10:00:00Z :to :notes :fn :add :args []}`)
	if err != nil || got.m.user || got.m.reviewed {
		t.Errorf("storedTimer = %+v %v, want an agent's, unreviewed", got, err)
	}
	for _, p := range []string{
		`[1]`,
		`{:to :notes :fn :add :args []}`,
		`{:due 2026-10-27T10:00:00Z :fn :add :args []}`,
		`{:due 2026-10-27T10:00:00Z :to :notes :args []}`,
		`{:due 2026-10-27T10:00:00Z :to :notes :fn :add}`,
	} {
		if got, err := storedTimer(nil, p); err == nil {
			t.Errorf("storedTimer(%s) = %+v, want an error", p, got)
		}
	}
}
