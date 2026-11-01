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

func TestMe(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"planner": `(service planner
  (on (plan) "Asks you whether to go ahead." (call :me/ask "go ahead?"))
  (on (report) "Tells you how it went." (call :me/tell "all done"))
  (on (sneak id) "Tries to answer for you." (reply id (ok "yes")))
  (on (whiff/done id r) "Writes your answer." (file/write "` + got + `" (value/to-string r))))`,
		"me": `(service me)`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	id := func(asAgent bool, src string) string {
		t.Helper()
		cl := c
		if asAgent {
			cl = agent
		}
		resp, err := cl.Eval(`(match `+src+` [(err {:kind :pending :id id}) id])`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	asked := id(true, `(call :planner/plan)`)
	told := id(false, `(call :planner/report)`)
	direct := id(false, `(call :me/ask "lunch?")`)

	text, err := c.Inbox("me")
	want := asked + "  :planner -> :me/ask \"go ahead?\"\n" + told + "  :planner -> :me/tell \"all done\"\n" + direct + "  client -> :me/ask \"lunch?\"\n"
	if err != nil || text != want {
		t.Errorf("inbox of :me = %q %v, want %q", text, err, want)
	}

	// Your answer reaches the asker; it stays the agent's chain.
	if _, err := c.Message("reply", "", "["+asked+" (ok \"yes\")]", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "your answer", func() string { b, _ := os.ReadFile(got); return string(b) }, `(ok "yes")`)
	historyHas(t, c, `  done  agent  client -> :planner/whiff/done `+asked+` (ok "yes")`)

	// A service answers only its own inbox, so it can't answer for you.
	resp, err := c.Eval(`(call :planner/sneak `+told+`)`, dir)
	if want := `(err {:kind :no-id :message "no message ` + told + ` waits in :planner's inbox"})`; err != nil || resp.Value != want {
		t.Errorf("a service answering :me's inbox = %s %v, want %s", resp.Value, err, want)
	}
	if text, _ := c.Inbox("me"); !strings.Contains(text, told+"  :planner -> :me/tell") {
		t.Errorf("the report left :me's inbox: %q", text)
	}

	// :me is whiff's: no client claims it and no service file takes it.
	if resp, err := c.Claim("me", ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:kind :taken") {
		t.Errorf("claim :me = %+v %v", resp, err)
	}
	historyHas(t, c, `me.puff:1:1: :me is whiff's own address; rename the file`)
}

// :me's inbox as a view, answered with :me/answer from your connection
// only: not an agent's, nor a service's in a chain you began.
func TestMeInbox(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"planner": `(service planner
  (on (plan) "Asks you whether to go ahead." (call :me/ask "go ahead?"))
  (on (report) "Tells you how it went." (call :me/tell "all done"))
  (on (sneak id) "Tries to answer for you." (call :me/answer id "yes"))
  (on (whiff/done id r) "Writes your answer." (file/write "` + got + `" (value/to-string r))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	watcher := dial(t, dir)
	if _, err := watcher.Sub("me/changed"); err != nil {
		t.Fatal(err)
	}
	changed := func(want string) {
		t.Helper()
		select {
		case e := <-watcher.Events():
			if e.Value != want {
				t.Errorf(":me/changed = %s, want %s", e.Value, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no :me/changed")
		}
	}
	call := func(c *rpc.Client, target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
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

	asked := id(call(c, "planner/plan", `[]`))
	changed("1")
	told := id(call(c, "planner/report", `[]`))
	changed("2")
	want := `(ok (items [{:actions [{:action [:me/answer ` + asked + `] :ask "Your answer" :label "Answer"}] :detail "` + asked + `: :planner asks" :label "go ahead?"} ` +
		`{:actions [{:action [:me/answer ` + told + `] :label "Dismiss"}] :detail "` + told + `: :planner tells you" :label "all done"}] {:changed :me/changed}))`
	if r := call(c, "me/inbox", `[]`); r != want {
		t.Errorf("me/inbox = %s, want %s", r, want)
	}

	// Only your connection answers: not an agent, nor a service you call.
	for _, r := range []string{call(agent, "me/inbox", `[]`), call(agent, "me/answer", "["+asked+` "yes"]`), call(c, "planner/sneak", "["+asked+"]")} {
		if !strings.Contains(r, ":kind :refused") {
			t.Errorf("an answer not from your connection = %s", r)
		}
	}

	// Your answer reaches the asker, as text; a report is dismissed.
	if r := call(c, "me/answer", "["+asked+` "yes"]`); r != "(ok :done)" {
		t.Errorf("me/answer = %s", r)
	}
	changed("1")
	eventually(t, "your answer", func() string { b, _ := os.ReadFile(got); return string(b) }, `(ok "yes")`)
	if r := call(c, "me/answer", "["+told+"]"); r != "(ok :done)" {
		t.Errorf("dismiss = %s", r)
	}
	changed("0")
	if r := call(c, "me/answer", "["+told+"]"); !strings.Contains(r, ":kind :no-id") {
		t.Errorf("dismiss again = %s", r)
	}
}

// A question offers its choices, and a report its actions; an agent's
// report's actions are labelled with their messages, since clicking one
// sends it as you.
func TestMeChoicesAndActions(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	call := func(c *rpc.Client, target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
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

	q := id(call(c, "me/ask", `["merge whiff/2?" {:choices ["yes" "no"]}]`))
	yours := id(call(c, "me/tell", `["plan 1 done" {:actions [{:label "Show the diff" :action [:me/inbox]}]}]`))
	agents := id(call(agent, "me/tell", `["plan 2 done" {:actions [{:label "Show the diff" :action [:me/answer 1 "x"]}]}]`))
	want := `(ok (items [` +
		`{:actions [{:action [:me/answer ` + q + ` "yes"] :label "yes"} {:action [:me/answer ` + q + ` "no"] :label "no"} {:action [:me/answer ` + q + `] :ask "Your answer" :label "Other"}] :detail "` + q + `: client asks" :label "merge whiff/2?"} ` +
		`{:actions [{:action [:me/inbox] :label "Show the diff"} {:action [:me/answer ` + yours + `] :label "Dismiss"}] :detail "` + yours + `: client tells you" :label "plan 1 done"} ` +
		`{:actions [{:action [:me/answer 1 "x"] :label "[:me/answer 1 \"x\"]"} {:action [:me/answer ` + agents + `] :label "Dismiss"}] :detail "` + agents + `: client tells you" :label "plan 2 done"}` +
		`] {:changed :me/changed}))`
	if r := call(c, "me/inbox", `[]`); r != want {
		t.Errorf("me/inbox = %s\nwant %s", r, want)
	}
	if r := call(c, "me/answer", "["+q+` "yes"]`); r != "(ok :done)" {
		t.Errorf("answer with a choice = %s", r)
	}

	// Bad opts fail at once, rather than waiting in your inbox.
	for _, tt := range [][2]string{
		{"me/ask", `["q" {:choices []}]`},
		{"me/ask", `["q" {:choices ["a" 1]}]`},
		{"me/ask", `["q" {:actions []}]`},
		{"me/ask", `["q" [:choices]]`},
		{"me/tell", `["t" {:actions [{:label "x"}]}]`},
		{"me/tell", `["t" {:choices ["a"]}]`},
		{"me/tell", `["t" {} 1]`},
	} {
		if r := call(c, tt[0], tt[1]); !strings.Contains(r, ":kind :bad-call") {
			t.Errorf("%s %s = %s", tt[0], tt[1], r)
		}
	}
}

// TestMeActedOn checks that a :write call you make that a report offers,
// and that answers (ok v), dismisses the report; one that shows you
// something, or fails, leaves it.
func TestMeActedOn(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{"jot": `(service jot
  (on (look) :read "Looks." (ok 1))
  (on (keep x) :write "Keeps x." (if (= x 0) (err {:kind :zero}) (ok x))))`})
	start(t, dir)
	c := dial(t, dir)
	call := func(target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	call("me/tell", `["one" {:actions [{:label "Look" :action [:jot/look]} {:label "Keep 1" :action [:jot/keep 1]}]}]`)
	call("me/tell", `["two" {:actions [{:label "Keep" :action [:jot/keep] :ask "What?"}]}]`)
	call("me/tell", `["three" {:actions [{:label "Keep 0" :action [:jot/keep 0]}]}]`)
	left := func() string {
		t.Helper()
		text, _ := c.Inbox("me")
		var labels []string
		for _, line := range strings.Split(text, "\n") {
			if _, rest, ok := strings.Cut(line, `:me/tell "`); ok {
				labels = append(labels, rest[:strings.Index(rest, `"`)])
			}
		}
		return strings.Join(labels, " ")
	}
	for _, tt := range [][3]string{
		{"jot/look", "[]", "one two three"},  // a :read leaves it
		{"jot/keep", "[0]", "one two three"}, // so does a failure
		{"jot/keep", "[1]", "two three"},     // done what it offered
		{"jot/keep", `["x"]`, "three"},       // with your answer
	} {
		call(tt[0], tt[1])
		if got := left(); got != tt[2] {
			t.Errorf("after %s %s, your inbox has %q, want %q", tt[0], tt[1], got, tt[2])
		}
	}
}

// What waits for you outlives the daemon, as who sent it: an agent's chain
// stays an agent's, and ids aren't given out twice.
func TestMeInboxSurvivesRestart(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"planner": `(service planner
  (on (ask) "Asks you whether to go ahead." (call :me/ask "go ahead?" {:choices ["yes" "no"]}))
  (on (report) "Reports, offering a merge." (call :me/tell "built" {:actions [{:label "Merge" :action [:planner/merge 3]}]}))
  (on (merge n) "Merges." (ok n))
  (on (pass id) "Hands on an ask." (hand-on (err {:kind :pending :id id})))
  (on (whiff/done id r) "Writes your answer." (file/write "` + got + `" (value/to-string r))))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	id := func(cl *rpc.Client, src string) string {
		t.Helper()
		resp, err := cl.Eval(`(match `+src+` [(err {:kind :pending :id id}) id])`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	restart := func(change func(db *bbolt.DB)) {
		t.Helper()
		if err := c.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		db, err := bbolt.Open(filepath.Join(dir, "whiff.db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		change(db)
		db.Close()
		done = start(t, dir)
		c = dial(t, dir)
	}
	stored := func(db *bbolt.DB, id string) string {
		t.Helper()
		n, _ := strconv.ParseInt(id, 10, 64)
		var p string
		db.View(func(tx *bbolt.Tx) error {
			p = string(tx.Bucket(meBucket).Get(idKey(n)))
			return nil
		})
		return p
	}

	asked := id(c, `(call :planner/ask)`)
	told := id(agent, `(call :planner/report)`)
	passed := id(c, `(call :planner/ask)`)
	if r, err := agent.Eval(`(call :planner/pass `+passed+`)`, dir); err != nil || !strings.Contains(r.Value, ":pending") {
		t.Fatalf("hand on: %+v %v", r, err)
	}
	inbox, err := c.Inbox("me")
	if err != nil || strings.Count(inbox, "\n") != 3 {
		t.Fatalf("inbox = %q %v", inbox, err)
	}

	restart(func(db *bbolt.DB) {
		// The hand-on took your chain's origin down to the agent's.
		for id, origin := range map[string]string{asked: ":origin :user", told: ":origin :agent", passed: ":origin :agent"} {
			if p := stored(db, id); !strings.Contains(p, origin) {
				t.Errorf("message %s is stored as %s, want %s", id, p, origin)
			}
		}
	})
	if text, err := c.Inbox("me"); err != nil || text != inbox {
		t.Errorf("inbox after a restart = %q %v, want %q", text, err, inbox)
	}
	// An agent's report still shows the message its action sends, not its label.
	r, err := c.Message("call", "me/inbox", "[]", "")
	if err != nil || !strings.Contains(r.Value, `:label "[:planner/merge 3]"`) || strings.Contains(r.Value, `"Merge"`) {
		t.Errorf("me/inbox = %+v %v", r, err)
	}

	// An answer reaches the sender, as a chain of its own.
	if _, err := c.Message("reply", "", "["+asked+` (ok "yes")]`, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "your answer", func() string { b, _ := os.ReadFile(got); return string(b) }, `(ok "yes")`)
	historyHas(t, c, `  done  user   client -> :planner/whiff/done `+asked+` (ok "yes")`)
	if text, _ := c.Inbox("me"); strings.Contains(text, asked+"  ") {
		t.Errorf("answered, it is still in the inbox: %q", text)
	}
	for _, id := range []string{told, passed} {
		if _, err := c.Message("reply", "", "["+id+" (ok :done)]", ""); err != nil {
			t.Fatal(err)
		}
	}

	// With the inbox empty, the next id still comes after the last, and an
	// answered message doesn't come back.
	last, _ := strconv.Atoi(passed)
	restart(func(db *bbolt.DB) {
		if p := stored(db, asked); p != "" {
			t.Errorf("answered, it is still stored: %s", p)
		}
	})
	if text, _ := c.Inbox("me"); text != "" {
		t.Errorf("inbox = %q, want it empty", text)
	}
	waits := id(c, `(call :planner/ask)`)
	if n, _ := strconv.Atoi(waits); n != last+1 {
		t.Errorf("the next id is %d, want %d", n, last+1)
	}

	// One that can't be read is noted and kept, and what comes after it
	// still loads.
	restart(func(db *bbolt.DB) {
		db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(meBucket).Put(idKey(0), []byte("(not read"))
		})
	})
	historyHas(t, c, "message 0 in :me's inbox can't be read, and stays stored")
	if text, _ := c.Inbox("me"); !strings.HasPrefix(text, waits+"  :planner -> :me/ask") {
		t.Errorf("inbox = %q, want %s in it", text, waits)
	}
	restart(func(db *bbolt.DB) {
		if p := stored(db, "0"); p != "(not read" {
			t.Errorf("the unreadable message is stored as %q", p)
		}
	})
}

func TestStoredMe(t *testing.T) {
	const fine = `{:id 1 :fn :ask :args ["q"] :origin :user :cwd "/x" :at 2026-10-27T10:00:00Z :from :plan}`
	a, err := storedMe(1, fine)
	if err != nil {
		t.Fatal(err)
	}
	if a.id != 1 || a.addr != "me" || a.fn != "ask" || a.from != "plan" || a.dir != "/x" || !a.user || a.at.Year() != 2026 {
		t.Errorf("storedMe = %+v", a)
	}
	if a.msg != 0 || a.session != "" {
		t.Errorf("message ids and sessions begin again with a daemon: %+v", a)
	}
	// Only a stored :user is yours.
	for _, p := range []string{
		`{:id 1 :fn :ask :args ["q"] :cwd "/x" :at 2026-10-27T10:00:00Z}`,
		`{:id 1 :fn :ask :args ["q"] :origin "user" :cwd "/x" :at 2026-10-27T10:00:00Z}`,
	} {
		if a, err := storedMe(1, p); err != nil || a.user {
			t.Errorf("storedMe(%s) = %+v %v, want an agent's", p, a, err)
		}
	}
	// meItem counts on what it reads being what :me/ask and :me/tell check.
	for _, p := range []string{
		`[1]`,
		`{:fn :answer :args ["q"]}`,
		`{:fn :ask :args []}`,
		`{:fn :ask :args ["q" 1 2]}`,
		`{:fn :ask :args ["q" 1]}`,
		`{:fn :tell :args ["q" {:actions 1}]}`,
	} {
		if a, err := storedMe(1, p); err == nil {
			t.Errorf("storedMe(%s) = %+v, want an error", p, a)
		}
	}
}
