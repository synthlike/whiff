package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestRepl(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	other := dial(t, dir)

	tests := []struct{ src, value, err string }{
		{`(def (twice x) (* 2 x))`, "", ""},
		{`(def n 21)`, "", ""},
		{`(io/println "hi") (twice n)`, "42", ""},
		{`(twice nope)`, "", "repl:1:8: unknown name nope"},
		{`(call :me/ask "still here?")`, `(err {:id 1 :kind :pending :message ":me/ask waits in :me's inbox for a reply"})`, ""},
	}
	for _, tt := range tests {
		resp, err := c.Repl(tt.src, "")
		if err != nil || resp.Value != tt.value || resp.Error != tt.err {
			t.Errorf("repl %s = %+v %v, want %q %q", tt.src, resp, err, tt.value, tt.err)
		}
	}
	// What the REPL defined is its own: not eval's, not another connection's.
	for _, try := range []func(string, string) (string, error){
		func(src, dir string) (string, error) { r, err := c.Eval(src, dir); return r.Error, err },
		func(src, dir string) (string, error) { r, err := other.Repl(src, dir); return r.Error, err },
	} {
		if e, err := try("(twice 1)", ""); err != nil || !strings.HasSuffix(e, "unknown name twice") {
			t.Errorf("(twice 1) elsewhere: %q %v, want unknown name", e, err)
		}
	}

	// A cancel stops what runs, and the REPL keeps what it had.
	done := make(chan string)
	go func() {
		resp, _ := c.Repl(`(loop [] (recur))`, "")
		done <- resp.Error
	}()
	// Cancel until it stops: the loop's request may not be sent yet, and a
	// cancel before it does nothing.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for stop := time.After(2 * time.Second); ; {
		if err := c.Cancel(); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-done:
			if e == "" {
				t.Errorf("a cancelled loop has no error")
			}
		case <-tick.C:
			continue
		case <-stop:
			t.Fatal("cancel didn't stop the loop")
		}
		break
	}
	if resp, err := c.Repl("(twice n)", ""); err != nil || resp.Value != "42" {
		t.Errorf("after the cancel = %+v %v", resp, err)
	}
	// A cancel with nothing running does nothing.
	c.Cancel()
	if resp, err := c.Repl("n", ""); err != nil || resp.Value != "21" {
		t.Errorf("after a stray cancel = %+v %v", resp, err)
	}
}
