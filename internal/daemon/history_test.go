package daemon

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHistory(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(srv greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name)))
  (on (relay name) "Greets name through a send." (send :hello name)))`,
		"broken": `(srv broken`,
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
		h, err := c.History()
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
