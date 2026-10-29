package daemon

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestHistory(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(service greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name)))
  (on (relay name) "Greets name through a send." (send :hello name)))`,
		"broken": `(service broken`,
	})
	start(t, dir)
	c := dial(t, dir)
	for _, src := range []string{`(call :greet/hello "ann")`, `(send :nobody/x 1)`, `(call :greet/relay "bob")`} {
		if got, err := c.Eval(src, ""); err != nil || got.Error != "" {
			t.Fatalf("%s: %v %s", src, err, got.Error)
		}
	}

	// Times and durations vary, so they're left out.
	at := regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d\.\d{3}  `)
	took := regexp.MustCompile(`(?m)  [0-9.]+(ns|µs|ms|s)$`)
	want := strings.Join([]string{
		`send  user   :greet -> :greet/hello "bob"  (ok "hello, bob")`,
		`call  user   client -> :greet/relay "bob"  (ok :done)`,
		`send  user   client -> :nobody/x 1  (err {:kind :no-address :message "no address :nobody"})`,
		`call  user   client -> :greet/hello "ann"  (ok "hello, ann")`,
		`note  ` + filepath.Join(dir, "services", "broken.puff") + `:1:1: no closing ')'`,
	}, "\n") + "\n"
	// relay's send is handled after relay has replied.
	var got string
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h, err := historyOf(c)
		if err != nil {
			t.Fatal(err)
		}
		got = took.ReplaceAllString(at.ReplaceAllString(h, ""), "")
		if got == want || time.Now().After(deadline) {
			break
		}
	}
	if got != want {
		t.Errorf("history:\n%s\nwant:\n%s", got, want)
	}
}

func TestTrace(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)

	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(service greet (on (hello name) "Greets name." (ok (string/concat "hello, " name))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	if _, err := c.Eval(`(call :greet/hello "ann")`, ""); err != nil {
		t.Fatal(err)
	}

	tr := dial(t, dir)
	es, err := tr.Trace()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(es); n == 0 || es[n-1].To != "greet/hello" || es[n-1].Args != `["ann"]` {
		t.Fatalf("history at the trace's start: %+v", es)
	}
	if _, err := tr.Trace(); err == nil || err.Error() != "this connection traces history already" {
		t.Errorf("a second trace: %v", err)
	}

	// next is the next new entry.
	next := func() rpc.Entry {
		t.Helper()
		select {
		case e := <-tr.Entries():
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no entry")
			return rpc.Entry{}
		}
	}
	if _, err := c.Eval(`(call :greet/hello "bob")`, ""); err != nil {
		t.Fatal(err)
	}
	e := next()
	if e.ID == 0 || e.Cause != 0 {
		t.Errorf("a client's call has id %d and cause %d, want an id and no cause", e.ID, e.Cause)
	}
	e.At, e.Took, e.ID = time.Time{}, 0, 0
	if want := (rpc.Entry{Kind: "call", Origin: "user", To: "greet/hello", Args: `["bob"]`, Reply: `(ok "hello, bob")`}); e != want {
		t.Errorf("entry:\n%+v\nwant\n%+v", e, want)
	}
	writeServices(t, dir, map[string]string{"broken": `(service broken`})
	if e := next(); e.Kind != "note" || !strings.HasSuffix(e.Note, "broken.puff:1:1: no closing ')'") || e.Origin != "" {
		t.Errorf("note: %+v", e)
	}
}

func TestChains(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"front": `(service front
  (on (greet name) "Tells of name, and greets them." (do (publish :front/greeted name) (call :greet/hello name))))`,
		"greet": `(service greet (on (hello name) "Greets name." (ok (string/concat "hello, " name))))`,
		"log":   `(service log (on :front/greeted (v) (ok v)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	if r, err := c.Message("call", "front/greet", `["ann"]`, ""); err != nil || r.Value != `(ok "hello, ann")` {
		t.Fatalf("front/greet: %+v %v", r, err)
	}

	// The publish is handled after front has replied.
	byTo := map[string]rpc.Entry{}
	for deadline := time.Now().Add(2 * time.Second); byTo["log on :front/greeted"].ID == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		es, err := c.History()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			byTo[e.To] = e
		}
	}
	front, hello, log := byTo["front/greet"], byTo["greet/hello"], byTo["log on :front/greeted"]
	if front.ID == 0 || front.Cause != 0 {
		t.Errorf("the client's call: id %d, cause %d; want an id and no cause", front.ID, front.Cause)
	}
	for _, e := range []rpc.Entry{hello, log} {
		if e.ID == 0 || e.ID == front.ID || e.Cause != front.ID {
			t.Errorf("%s: id %d, cause %d; want its own id and cause %d", e.To, e.ID, e.Cause, front.ID)
		}
	}
	if hello.ID == log.ID {
		t.Errorf("the call and the publish share id %d", hello.ID)
	}
}
