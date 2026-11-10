package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestReload(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)

	dir := tempDir(t)
	services := filepath.Join(dir, "services")
	greet := filepath.Join(services, "greet.puff")
	version := 0
	// write writes a service file with a newer modification time each time,
	// so a change shows even within the file system's time resolution.
	write := func(path, src string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		version++
		at := time.Now().Add(time.Duration(version) * time.Second)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeServices(t, dir, nil)
	write(greet, `(service greet (on (hello name) "Greets name." (ok (string/concat "hello, " name))))`)
	start(t, dir)
	c := dial(t, dir)

	// becomes waits until src evaluates to want.
	becomes := func(src, want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			resp, err := c.Eval(src, "")
			if err != nil {
				t.Fatal(err)
			}
			got = resp.Value + resp.Error
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s = %s, want %s", src, got, want)
			}
		}
	}
	hello := `(call :greet/hello "ann")`
	becomes(hello, `(ok "hello, ann")`)

	write(greet, `(service greet (on (hello name) "Greets name." (ok (string/concat "hi, " name))))`)
	becomes(hello, `(ok "hi, ann")`)

	// A file that doesn't load leaves the running version.
	write(greet, `(service greet (on (hello name) "Greets name."`)
	historyHas(t, c, greet+":1:16: no closing ')'; :greet keeps running as it was")
	becomes(hello, `(ok "hi, ann")`)

	// New files load; removed ones take their service away.
	late := filepath.Join(services, "late.puff")
	write(late, `(service late (on (here) "Answers." (ok :here)))`)
	becomes(`(call :late/here)`, `(ok :here)`)
	if err := os.Remove(late); err != nil {
		t.Fatal(err)
	}
	becomes(`(match (call :late/here) [(err {:kind k}) k] [_ :still-there])`, ":no-address")
}

func TestRestartAfterFault(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"boom": `(service boom
  (on (boom) "Faults." (+ 1 1.0))
  (on (fine) "Answers." (ok :fine)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	got, err := c.Eval(`(match (call :boom/boom) [(err {:kind k}) k])`, "")
	if err != nil || got.Value != ":fault" {
		t.Fatalf("boom = %v %+v, want :fault", err, got)
	}
	historyHas(t, c, "  note  :boom faulted; restarting it from its file")
	got, err = c.Eval(`(call :boom/fine)`, "")
	if err != nil || got.Value != "(ok :fine)" {
		t.Errorf("fine after the restart = %v %+v, want (ok :fine)", err, got)
	}
}

// historyOf is c's history as whiff history prints it.
func historyOf(c *rpc.Client) (string, error) {
	es, err := c.History()
	return rpc.HistoryText(es), err
}

// historyHas waits until a line of history holds want.
func historyHas(t *testing.T, c *rpc.Client, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h, err := historyOf(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("history:\n%s\nwant a line with %s", h, want)
		}
	}
}
