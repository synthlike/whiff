package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	first := `(err {:kind :expired :message ":box/hour waited longer than 1h0m0s" :outcome :not-started})` + "\n"
	eventually(t, "the ask that expired while whiff was down", answers, first)
	if text, _ := c.Inbox("box"); !strings.Contains(text, q+"  :asker -> :box/q") || strings.Contains(text, hour+"  ") {
		t.Errorf("inbox of :box = %q", text)
	}
	eventually(t, "the ask that expired after", answers, first+`(err {:kind :expired :message ":box/later waited longer than 1s" :outcome :not-started})`+"\n")
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
	done = start(t, dir)
	c = dial(t, dir)
	before := answers()
	if _, err := c.Message("reply", "", "["+q+` (ok "yes")]`, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "your answer", answers, before+`(ok "yes")`+"\n")

	// Its result outlives the daemon too, for whiff wait.
	stopDaemon(t, c, done)
	start(t, dir)
	c = dial(t, dir)
	for id, want := range map[string]string{q: `(ok "yes")`, hour: first[:len(first)-1]} {
		if r, err := c.Message("wait", "", "["+id+"]", ""); err != nil || r.Value != want {
			t.Errorf("wait %s after a restart = %+v %v, want %s", id, r, err, want)
		}
	}
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
					due[string(tm.key)] = tm
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

// A job doesn't outlive the daemon, but its sender hears how it ended, as
// far as whiff knows, and it isn't run again.
func TestJobsInterrupted(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"waiter": `(service waiter
  (on (begin) "Starts a job." (call :jobs/run ["sleep" "30"]))
  (on (quick) "Starts a job that ends at once." (call :jobs/run ["true"] {:as :text}))
  (on (pass id) :read "Hands a job on." (hand-on (err {:kind :pending :id id})))
  (on (whiff/done id r) "Notes the answer." (file/append "` + got + `" (string/concat (int/to-string id) " " (value/to-string r) "\n"))))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	answers := func() string {
		b, _ := os.ReadFile(got)
		return string(b)
	}
	begin := func() string {
		t.Helper()
		r, err := c.Message("call", "waiter/begin", "[]", dir)
		if err != nil {
			t.Fatal(err)
		}
		return pendingOf(t, r.Value)
	}
	// One that ended is told once, and its result stays stored.
	r, err := c.Message("call", "waiter/quick", "[]", dir)
	if err != nil {
		t.Fatal(err)
	}
	quickID := pendingOf(t, r.Value)
	quick := quickID + ` (ok "")` + "\n"
	eventually(t, "the job that ended", answers, quick)
	mine, passed := begin(), begin()
	if r, err := agent.Message("call", "waiter/pass", "["+passed+"]", dir); err != nil || !strings.Contains(r.Value, ":pending") {
		t.Fatalf("hand on: %+v %v", r, err)
	}
	stopDaemon(t, c, done)

	stored := func(db *bbolt.DB, id string) (j *job) {
		t.Helper()
		n, _ := strconv.ParseInt(id, 10, 64)
		err := db.View(func(tx *bbolt.Tx) (err error) {
			j, err = storedJob(n, string(tx.Bucket(tasksBucket).Get(idKey(n))))
			return err
		})
		if err != nil {
			t.Fatalf("job %s: %v", id, err)
		}
		return j
	}
	withDB(t, dir, func(db *bbolt.DB) {
		n, _ := strconv.ParseInt(mine, 10, 64)
		if o, _, r, err := storedResult(db, n); err != nil || !strings.Contains(puff.Print(r), ":outcome :stopped") || !o.user {
			t.Errorf("job %s is stored as %+v %s %v", mine, o, puff.Print(r), err)
		}
		if j := stored(db, mine); !errors.Is(syscall.Kill(-j.pid, 0), syscall.ESRCH) {
			t.Errorf("job %s still runs", mine)
		}
		// The hand-on took your chain's origin down to the agent's.
		if j := stored(db, passed); j.user || j.from != "" {
			t.Errorf("job %s is stored as %+v, want the agent client's", passed, j)
		}
	})

	done = start(t, dir)
	c = dial(t, dir)
	first := quick + mine + ` (err {:kind :interrupted :message "job ` + mine + ` was killed when whiff stopped: what it had done stays done" :outcome :stopped})` + "\n"
	eventually(t, "the job whiff killed", answers, first)
	if r, err := c.Message("wait", "", "["+passed+"]", dir); err != nil || !strings.Contains(r.Value, ":interrupted") {
		t.Errorf("wait for a job handed on: %+v %v", r, err)
	}
	if r, err := c.Message("wait", "", "["+quickID+"]", dir); err != nil || r.Value != `(ok "")` {
		t.Errorf("wait for a job that ended before the restart: %+v %v", r, err)
	}

	// What a crash leaves: no answer, and a process that may still run.
	stopDaemon(t, c, done)
	running := exec.Command("sleep", "30")
	running.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-running.Process.Pid, syscall.SIGKILL)
		running.Wait()
	})
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	cmd := puff.NewVector(puff.String("x"))
	sender := ticket{from: "waiter", user: true}
	jobs := []*job{
		{ticket: sender, cmd: cmd},
		{ticket: sender, cmd: cmd, pid: gone.Process.Pid},
		{ticket: sender, cmd: cmd, pid: running.Process.Pid},
		{ticket: sender, cmd: cmd},
	}
	// 950 ended long ago: its result goes at start.
	old, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("status"), Val: puff.Keyword("ended")},
		puff.Entry{Key: puff.Keyword("ended"), Val: puff.NewTime(time.Now().Add(-resultKept - time.Hour))},
		puff.Entry{Key: puff.Keyword("result"), Val: puff.Ok(puff.Int(1))},
		puff.Entry{Key: puff.Keyword("handled"), Val: puff.Bool(true)},
	)
	withDB(t, dir, func(db *bbolt.DB) {
		err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(tasksBucket)
			for i, j := range jobs {
				j.id = int64(900 + i)
				if err := b.Put(idKey(j.id), []byte(puff.Print(j.stored()))); err != nil {
					return err
				}
			}
			if err := b.Put(idKey(950), []byte(puff.Print(old))); err != nil {
				return err
			}
			return b.Put(idKey(990), []byte("(not read"))
		})
		if err != nil {
			t.Fatal(err)
		}
		// 900 finished as whiff stopped, before its sender was told.
		if err := endTask(db, jobs[0].ticket, "jobs", puff.Ok(puff.String("it finished")), false); err != nil {
			t.Fatal(err)
		}
	})
	done = start(t, dir)
	c = dial(t, dir)
	unknown := func(id int) string {
		return fmt.Sprintf(`%d (err {:kind :interrupted :message "job %d was running when whiff died: how far it got is unknown" :outcome :unknown})`, id, id) + "\n"
	}
	want := first + `900 (ok "it finished")` + "\n" + unknown(901) +
		fmt.Sprintf(`902 (err {:kind :interrupted :message "job 902 was running when whiff died: how far it got is unknown, and process group %d may still be running" :outcome :unknown :pid %d})`,
			running.Process.Pid, running.Process.Pid) + "\n" + unknown(903)
	eventually(t, "the jobs a crash left", answers, want)
	historyHas(t, c, "task 990 can't be read, and stays stored")
	if r, err := c.Message("wait", "", "[950]", dir); err != nil || !strings.Contains(r.Value, ":no-id") {
		t.Errorf("wait for a result past its time: %+v %v", r, err)
	}
	if err := syscall.Kill(-running.Process.Pid, 0); err != nil {
		t.Errorf("whiff killed what may not be its own: %v", err)
	}
	// Ids go on after the last job's, read or not.
	if id := begin(); id != "991" {
		t.Errorf("the next id is %s, want 991", id)
	}

	// Each sender hears once.
	stopDaemon(t, c, done)
	withDB(t, dir, func(db *bbolt.DB) {
		for _, j := range jobs {
			db.View(func(tx *bbolt.Tx) error {
				p := tx.Bucket(tasksBucket).Get(idKey(j.id))
				if get, err := fields(string(p)); err != nil || !puff.Equal(get("handled"), puff.Bool(true)) {
					t.Errorf("job %d, answered, is stored as %s", j.id, p)
				}
				return nil
			})
		}
	})
	start(t, dir)
	c = dial(t, dir)
	eventually(t, "job 991, killed by the stop", func() string {
		return strconv.FormatBool(strings.Contains(answers(), "991 (err {:kind :interrupted"))
	}, "true")
	if a := answers(); strings.Count(a, "\n") != 7 {
		t.Errorf("answers:\n%s", a)
	}
}

// A result a service hadn't handled when whiff went comes to it at the next
// start, or once it loads, and only once: a stateful service's mark commits
// with its state. One it faults on isn't given again, and one for an owner
// with no file is a line in history.
func TestResultsUntilHandled(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"counter": `(service counter :state 0
  (on (get n) "The count." (ok n))
  (on (whiff/done n id r) :update "Counts results." (ok (+ n 1))))`,
		"faulty": `(service faulty (on (whiff/done id r) "Faults." (+ 1 1.0)))`,
		"later":  `(service later`,
	})
	db, err := openStore(filepath.Join(dir, "services"))
	if err != nil {
		t.Fatal(err)
	}
	for i, owner := range []string{"counter", "faulty", "later", "gone"} {
		if err := endTask(db, ticket{id: int64(500 + i), from: owner, user: true}, "jobs", puff.Ok(puff.Int(i)), false); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	done := start(t, dir)
	c := dial(t, dir)
	count := func() string {
		resp, err := c.Eval(`(call :counter/get)`, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	eventually(t, "counter's count", count, "(ok 1)")
	historyHas(t, c, ":faulty faulted on the result of task 501, so it won't get it again")
	historyHas(t, c, "1 waiting for :later, which isn't loaded")
	writeServices(t, dir, map[string]string{"later": `(service later
  (on (whiff/done id r) "Notes it." (file/append "` + got + `" (string/concat (int/to-string id) "\n"))))`})
	eventually(t, ":later, loaded", func() string {
		b, _ := os.ReadFile(got)
		return string(b)
	}, "502\n")
	stopDaemon(t, c, done)

	withDB(t, dir, func(db *bbolt.DB) {
		db.View(func(tx *bbolt.Tx) error {
			for id := 500; id < 504; id++ {
				p := tx.Bucket(tasksBucket).Get(idKey(int64(id)))
				if get, err := fields(string(p)); err != nil || !puff.Equal(get("handled"), puff.Bool(true)) {
					t.Errorf("task %d is stored as %s", id, p)
				}
			}
			return nil
		})
	})
	start(t, dir)
	c = dial(t, dir)
	if n := count(); n != "(ok 1)" {
		t.Errorf("after a restart, counter's count is %s: it got a result twice", n)
	}
	if b, _ := os.ReadFile(got); string(b) != "502\n" {
		t.Errorf(":later got %q", b)
	}
}

func TestStoredJob(t *testing.T) {
	j := &job{ticket: ticket{id: 7, from: "plan", dir: "/x", user: true, session: "agent/2"}, cmd: puff.NewVector(puff.String("go")), pid: 42}
	got, err := storedJob(7, puff.Print(j.stored()))
	if err != nil {
		t.Fatal(err)
	}
	if got.id != 7 || got.from != "plan" || got.dir != "/x" || !got.user || got.session != "agent/2" || got.pid != 42 ||
		puff.Print(got.cmd) != `["go"]` {
		t.Errorf("storedJob = %+v", got)
	}
	// Only a stored :user is yours.
	got, err = storedJob(7, `{:cmd ["go"]}`)
	if err != nil || got.user {
		t.Errorf("storedJob = %+v %v", got, err)
	}
	for _, p := range []string{`[1]`, `{:cwd "/x"}`, `(not read`} {
		if got, err := storedJob(7, p); err == nil {
			t.Errorf("storedJob(%s) = %+v, want an error", p, got)
		}
	}
	// A record with no pid doesn't signal whiff's own group.
	if r := puff.Print(lost(&job{ticket: ticket{id: 1}})); strings.Contains(r, ":pid") {
		t.Errorf("lost = %s", r)
	}
}
