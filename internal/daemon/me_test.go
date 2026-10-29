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
