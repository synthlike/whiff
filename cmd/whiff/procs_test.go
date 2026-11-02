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
	// a shuts down cleanly on SIGTERM; b writes its pid, its group's.
	table := `(def procs [:demo/a {:cmd ["sh" "-c" "echo a up; trap 'echo a bye; exit 0' TERM; while :; do sleep 0.1; done"]
                            :dir "` + dir + `" :stop ["sh" "-c" "echo stopped >> stops"]}
                    :demo/b {:cmd ["sh" "-c" "echo $$ > b.pid; echo b up; while :; do sleep 0.1; done"] :dir "` + dir + `"}
                    :demo/flaky {:cmd ["sh" "-c" "echo flaky; exit 3"] :restart #t}])`
	s := string(src)
	for old, new := range map[string]string{"(def procs [])": table, "(def delay 2s)": "(def delay 50ms)"} {
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

	if got := call("procs/up", "[:demo]"); got != "(ok {:demo/a :running :demo/b :running :demo/flaky :running})" {
		t.Fatalf("up = %s", got)
	}
	until("a's output", func() bool { return strings.Contains(call("procs/logs", "[:demo/a]"), "a up") })
	// flaky exits, and is started again, until its third exit.
	until("flaky giving up", runs(":proc :demo/flaky :status :failed"))
	if inbox := call("me/inbox", "[]"); !strings.Contains(inbox, "procs: demo/flaky exited 3 times in a minute") ||
		!strings.Contains(inbox, "[:procs/up :demo/flaky]") {
		t.Errorf("your inbox = %s", inbox)
	}
	if view := call("procs/view", "[]"); !strings.Contains(view, "[:procs/restart :demo/a]") || !strings.Contains(view, "[:procs/up :demo/flaky]") {
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
	if got := call("procs/up", "[:demo/b]"); got != "(ok {:demo/b :stray})" {
		t.Errorf("up while b's group runs = %s", got)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	until("b's group gone", func() bool { return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) })
	if got := call("procs/up", "[:demo/b]"); got != "(ok {:demo/b :running})" {
		t.Errorf("up once b's group is gone = %s", got)
	}
}
