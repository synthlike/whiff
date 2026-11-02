package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// TestProcs runs services/procs.puff on a real daemon, with two servers
// and one that keeps exiting. They start in order; the one that exits is
// started again until its third exit in a minute, which you hear of; whiff
// stop and start brings back the ones you wanted up; down runs a :stop; and
// after a crash, one whose process group still runs isn't started again
// until it is gone.
func TestProcs(t *testing.T) {
	if testing.Short() {
		t.Skip("stops, kills and restarts a daemon")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "procs.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// a shuts down cleanly on SIGTERM, and is ready once a.ready is there,
	// a moment after it starts; b comes after it, and writes its pid, its
	// group's, and early if a wasn't ready. slow is never ready.
	table := `(def procs [:demo/a {:cmd ["sh" "-c" "echo a up; (sleep 0.2; touch a.ready) & trap 'echo a bye; exit 0' TERM; while :; do sleep 0.1; done"]
                            :dir "` + dir + `" :stop ["sh" "-c" "echo stopped >> stops"] :ready ["test" "-e" "a.ready"] :url "http://localhost:1/a"}
                    :demo/b {:cmd ["sh" "-c" "test -e a.ready || touch early; echo $$ > b.pid; echo b up; while :; do sleep 0.1; done"]
                             :dir "` + dir + `" :after [:demo/a]}
                    :demo/flaky {:cmd ["sh" "-c" "echo flaky; exit 3"] :restart #t}
                    :other/slow {:cmd ["sleep" "60"] :ready ["false"]}
                    :health/again {:cmd ["sh" "-c" "touch again.ok; echo run >> again.runs; while :; do sleep 0.1; done"]
                                   :dir "` + dir + `" :ready ["test" "-e" "again.ok"] :restart #t}
                    :health/sick {:cmd ["sh" "-c" "touch sick.ok; while :; do sleep 0.1; done"] :dir "` + dir + `" :ready ["test" "-e" "sick.ok"]}])`
	s := string(src)
	for old, new := range map[string]string{"(def procs [])": table, "(def delay 2s)": "(def delay 50ms)",
		"(def every 1s)": "(def every 50ms)", "(def ready-within 2m)": "(def ready-within 1s)", "(def checking 10s)": "(def checking 50ms)"} {
		if strings.Count(s, old) != 1 {
			t.Fatalf("procs.puff should have %s once", old)
		}
		s = strings.Replace(s, old, new, 1)
	}
	os.Mkdir(filepath.Join(dir, "services"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "services", "procs.puff"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	// daemon starts whiff; the test's cleanup stops it, so its jobs stop
	// too, before testDaemon's kills it.
	daemon := func() (*exec.Cmd, *rpc.Client) {
		cmd, c := testDaemon(t, dir, "HOME="+dir)
		t.Cleanup(func() { c.Stop() })
		return cmd, c
	}
	cmd, c := daemon()
	call := func(fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", fn, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: runs = %s", what, call("procs/runs", "[]"))
			}
		}
	}
	runs := func(want ...string) func() bool {
		return func() bool {
			got := call("procs/runs", "[]")
			for _, w := range want {
				if !strings.Contains(got, w) {
					return false
				}
			}
			return true
		}
	}
	// job is name's job id, as runs says.
	job := func(name string) string {
		t.Helper()
		got := call("procs/runs", "[]")
		m := regexp.MustCompile(`:job (\d+) (?::pid \d+ )?:proc :demo/` + name + ` `).FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("no job for %s: %s", name, got)
		}
		return m[1]
	}

	// a starts until its :ready passes; b, after it, waits for that.
	if got := call("procs/up", "[:demo]"); got != "(ok {:demo/a :starting :demo/b :queued :demo/flaky :running})" {
		t.Fatalf("up = %s", got)
	}
	until("b after a", runs(":proc :demo/a :status :running", ":proc :demo/b :status :running"))
	if _, err := os.Stat(filepath.Join(dir, "early")); err == nil {
		t.Error("b started before a was ready")
	}
	until("a's output", func() bool { return strings.Contains(call("procs/logs", "[:demo/a]"), "a up") })
	// One whose :ready never passes is :unready, and you hear of it.
	call("procs/up", "[:other]")
	until("slow unready", runs(":proc :other/slow :status :unready"))
	if inbox := call("me/inbox", "[]"); !strings.Contains(inbox, "procs: other/slow started, but its :ready didn't pass within 1s") {
		t.Errorf("your inbox = %s", inbox)
	}
	call("procs/down", "[:other]")
	until("slow down", runs(":proc :other/slow :status :down"))
	// A running one whose :ready stops passing, 3 times in a row: with
	// :restart #t it is started again; without, it is :unready until it
	// passes again.
	call("procs/up", "[:health]")
	until("health up", runs(":proc :health/again :status :running", ":proc :health/sick :status :running"))
	os.Remove(filepath.Join(dir, "again.ok"))
	os.Remove(filepath.Join(dir, "sick.ok"))
	until("sick unready", runs(":proc :health/sick :status :unready"))
	until("again restarted", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "again.runs"))
		return string(b) == "run\nrun\n"
	})
	until("again back", runs(":proc :health/again :status :running"))
	if inbox := call("me/inbox", "[]"); !strings.Contains(inbox, "procs: health/again failed its :ready 3 times in a row, so it was restarted.") ||
		!strings.Contains(inbox, "procs: health/sick failed its :ready 3 times in a row.") {
		t.Errorf("your inbox = %s", inbox)
	}
	os.WriteFile(filepath.Join(dir, "sick.ok"), nil, 0o600)
	until("sick well", runs(":proc :health/sick :status :running"))
	call("procs/down", "[:health]")
	until("health down", runs(":proc :health/again :status :down", ":proc :health/sick :status :down"))
	// flaky exits, and is started again, until its third exit.
	until("flaky giving up", runs(":proc :demo/flaky :status :failed"))
	if inbox := call("me/inbox", "[]"); !strings.Contains(inbox, "procs: demo/flaky exited 3 times in a minute") ||
		!strings.Contains(inbox, "[:procs/up :demo/flaky]") {
		t.Errorf("your inbox = %s", inbox)
	}
	if view := call("procs/view", "[]"); !strings.Contains(view, "[:procs/restart :demo/a]") || !strings.Contains(view, "[:procs/up :demo/flaky]") ||
		!strings.Contains(view, `:url "http://localhost:1/a"`) {
		t.Errorf("view = %s", view)
	}

	// whiff stop stops them; starting again brings back a and b, not flaky.
	a := job("a")
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	cmd, c = daemon()
	until("a and b back", runs(":proc :demo/a :status :running", ":proc :demo/b :status :running", ":proc :demo/flaky :status :failed"))
	if job("a") == a {
		t.Errorf("a's job is still %s after a restart", a)
	}

	// down stops a, then runs its :stop.
	call("procs/down", "[:demo/a]")
	until("a down", runs(":proc :demo/a :status :down"))
	if b, _ := os.ReadFile(filepath.Join(dir, "stops")); string(b) != "stopped\n" {
		t.Errorf("a's :stop wrote %q", b)
	}
	// Its logs keep what it printed, as it shut down too.
	if logs := call("procs/logs", "[:demo/a]"); !strings.Contains(logs, `a up\na bye`) {
		t.Errorf("a's logs once down = %s, want its last output", logs)
	}

	// whiff dies; b's group outlives it, so b isn't started again until
	// you stop it; a stays down.
	b, _ := os.ReadFile(filepath.Join(dir, "b.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, c = daemon()
	group := strconv.Itoa(pid)
	until("b stray", runs(":pid "+group+" :proc :demo/b :status :stray", ":proc :demo/a :status :down"))
	if inbox := call("me/inbox", "[]"); !strings.Contains(inbox, "kill -- -"+group) {
		t.Errorf("your inbox = %s", inbox)
	}
	// up b brings a, which b comes after, too.
	if got := call("procs/up", "[:demo/b]"); got != "(ok {:demo/a :starting :demo/b :stray})" {
		t.Errorf("up while b's group runs = %s", got)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	until("b's group gone", func() bool { return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) })
	call("procs/up", "[:demo/b]")
	until("b up once its group is gone", runs(":proc :demo/a :status :running", ":proc :demo/b :status :running"))
}
