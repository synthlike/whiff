package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStart(t *testing.T) {
	dir := tempDir(t)
	count := filepath.Join(dir, "count")
	writeServices(t, dir, map[string]string{
		"ticker": `(service ticker :start (fn () (send-after 50ms :tick 1))
  (on (tick n) "Counts to 3."
    (do (file/write "` + count + `" (int/to-string n))
        (if (< n 3) (send-after 10ms :tick (+ n 1)) (ok :done)))))`,
		"bad": `(service bad :start (fn () (err {:kind :nope})))`,
	})
	start(t, dir)
	c := dial(t, dir)

	eventually(t, "the count", func() string { b, _ := os.ReadFile(count); return string(b) }, "3")
	// :start runs as the user.
	historyHas(t, c, `  send  user   :ticker -> :ticker/tick 1  (ok :done)`)
	historyHas(t, c, filepath.Join(dir, "services", "bad.puff")+`:1:21: :start returned (err {:kind :nope}), want (ok v)`)
	if resp, err := c.Eval(`(match (call :bad/x) [(err {:kind k}) k])`, ""); err != nil || resp.Value != ":no-address" {
		t.Errorf("a service whose :start failed: %s %s %v", resp.Value, resp.Error, err)
	}
}

func TestStartReload(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)

	dir := tempDir(t)
	file := filepath.Join(dir, "services", "mark.puff")
	// version writes the service: its :start marks a file named v after d.
	version := func(v, d string, at time.Time) {
		t.Helper()
		src := `(service mark :start (fn () (send-after ` + d + ` :mark "` + v + `"))
  (on (mark v) "Marks v." (file/write (path/join "` + dir + `" v) "")))`
		if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeServices(t, dir, nil)
	version("v1", "300ms", time.Now())
	done := start(t, dir)
	c := dial(t, dir)
	// The daemon stops before pollInterval is restored.
	defer func() {
		if err := c.Stop(); err != nil {
			t.Error(err)
		}
		wait(t, done)
	}()

	// v2 replaces v1 before v1's timer fires: v1's timer goes with it.
	version("v2", "10ms", time.Now().Add(time.Second))
	exists := func(v string) string {
		if _, err := os.Stat(filepath.Join(dir, v)); err != nil {
			return "missing"
		}
		return "there"
	}
	eventually(t, "v2's mark", func() string { return exists("v2") }, "there")
	time.Sleep(400 * time.Millisecond)
	if got := exists("v1"); got != "missing" {
		t.Errorf("v1's mark after v2 replaced it: %s, want missing", got)
	}
}

// A timer :start sets for its service that comes due before the service
// has its address waits for it, rather than failing :no-address: so a
// loop's first tick isn't lost to a slow load. TestGhWatch lost gh's
// tick so under load.
func TestStartTimerBeforeAddress(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"loop": `(service loop :state 0
  :start (fn (n) (do (send-after 0s :tick) (proc/run ["sleep" "0.3"]) (ok n)))
  (on (tick n) :update "Counts." (ok [n (+ n 1)]))
  (on (count n) "The ticks." (ok n)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	eventually(t, "the tick", func() string {
		resp, _ := c.Eval(`(call :loop/count)`, "")
		return resp.Value
	}, "(ok 1)")
	if h, _ := historyOf(c); strings.Contains(h, "no-address") {
		t.Errorf("history:\n%s\nwant no :no-address", h)
	}
}
