package daemon

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestTimers lists and views the timers waiting, and cancels one, which
// then never fires; cancel is yours.
func TestTimers(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"pinger": `(service pinger
  (on (start) :write "Pings itself soon and later." (do (send-after 300ms :pinger/ping "soon") (send-after 1h :pinger/ping "later") (ok :done)))
  (on (ping what) :write "Hears a ping." (call :me/tell what)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	watch := dial(t, dir)
	if _, err := watch.Sub("timers/changed"); err != nil {
		t.Fatal(err)
	}
	call := func(target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	call("pinger/start", "[]")
	list := call("timers/list", "[]")
	m := regexp.MustCompile(`^\(ok \[\{:args \["soon"\] :due \S+ :fn :ping :from :pinger :id (\d+) :to :pinger\} \{:args \["later"\] :due \S+ :fn :ping :from :pinger :id (\d+) :to :pinger\}\]\)$`).FindStringSubmatch(list)
	if m == nil {
		t.Fatalf("timers/list = %s", list)
	}
	later := m[2]
	view := call("timers/view", "[]")
	for _, want := range []string{
		`{:actions [{:action [:timers/cancel ` + later + `] :label "Cancel"}] :detail "set by itself · `,
		`· [\"later\"]" :label "in 1h  :pinger/ping"}`,
		`{:changed :timers/changed}`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("timers/view lacks %s:\n%s", want, view)
		}
	}

	// The soon one fires, and goes.
	eventually(t, "the soon timer, fired", func() string {
		text, _ := c.Inbox("me")
		return fmt.Sprint(strings.Contains(text, "soon"))
	}, "true")
	eventually(t, "the timers left", func() string {
		return fmt.Sprint(strings.HasPrefix(call("timers/list", "[]"), `(ok [{:args ["later"]`))
	}, "true")

	// Cancel is yours.
	if r, _ := agent.Message("call", "timers/cancel", "["+later+"]", dir); !strings.HasPrefix(r.Value, "(err") {
		t.Errorf("an agent's timers/cancel = %s", r.Value)
	}
	if r := call("timers/cancel", "["+later+"]"); r != "(ok :done)" {
		t.Errorf("timers/cancel = %s", r)
	}
	if r := call("timers/list", "[]"); r != "(ok [])" {
		t.Errorf("timers/list after cancel = %s", r)
	}
	if r := call("timers/cancel", "["+later+"]"); !strings.Contains(r, ":no-id") {
		t.Errorf("timers/cancel again = %s", r)
	}
	select {
	case e := <-watch.Events():
		if e.Topic != "timers/changed" {
			t.Errorf("event %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Error("no :timers/changed")
	}
}

// TestTimerCancelled cancels a timer before it is due: it never sends.
func TestTimerCancelled(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"pinger": `(service pinger (on (ping) :write "Hears a ping." (call :me/tell "pinged")))`,
	})
	start(t, dir)
	c := dial(t, dir)
	if r, _ := c.Eval(`(send-after 200ms :pinger/ping)`, dir); r.Value != "(ok :done)" {
		t.Fatalf("send-after = %+v", r)
	}
	resp, _ := c.Message("call", "timers/list", "[]", dir)
	m := regexp.MustCompile(`:id (\d+)`).FindStringSubmatch(resp.Value)
	if m == nil {
		t.Fatalf("timers/list = %s", resp.Value)
	}
	if r, _ := c.Message("call", "timers/cancel", "["+m[1]+"]", dir); r.Value != "(ok :done)" {
		t.Fatalf("timers/cancel = %s", r.Value)
	}
	time.Sleep(500 * time.Millisecond)
	if text, _ := c.Inbox("me"); strings.Contains(text, "pinged") {
		t.Errorf("the cancelled timer fired:\n%s", text)
	}
}
