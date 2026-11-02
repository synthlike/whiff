package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// A job that asked to runs again under its id after a timeout, whiff
// stopping or whiff dying, if running it twice is safe, until its attempts
// run out; its sender sees one result, the last.
func TestRetries(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, nil)
	done := start(t, dir)
	c := dial(t, dir)
	run := func(args string) string {
		t.Helper()
		r, err := c.Message("call", "jobs/run", args, dir)
		if err != nil {
			t.Fatal(err)
		}
		return pendingOf(t, r.Value)
	}
	wait := func(id string) string {
		t.Helper()
		r, err := c.Message("wait", "", "["+id+"]", dir)
		if err != nil {
			t.Fatal(err)
		}
		return r.Value
	}
	// tries is a script that notes each attempt in file and sleeps on
	// those before the nth.
	tries := func(file string, n int) string {
		return `["sh" "-c" "echo x >> ` + file + `; [ $(wc -l < ` + file + `) -ge ` + strconv.Itoa(n) + ` ] || sleep 30"]`
	}
	attempts := func(file string) int {
		b, _ := os.ReadFile(file)
		return strings.Count(string(b), "\n")
	}
	ok := `(ok {:exit 0 :stderr "" :stdout ""})`

	// A timeout, then it finishes.
	f := filepath.Join(dir, "a")
	id := run(`[` + tries(f, 2) + ` {:timeout 200ms :retry 2 :idempotent #t}]`)
	if r := wait(id); r != ok || attempts(f) != 2 {
		t.Errorf("job %s = %s after %d attempts, want %s after 2", id, r, attempts(f), ok)
	}
	historyHas(t, c, "job "+id+": job "+id+" took longer than 200ms; attempt 2 of 3")

	// A write that doesn't say it's idempotent runs once.
	f = filepath.Join(dir, "b")
	id = run(`[` + tries(f, 2) + ` {:timeout 200ms :retry 2}]`)
	if r := wait(id); !strings.Contains(r, ":kind :timeout") || attempts(f) != 1 {
		t.Errorf("job %s = %s after %d attempts, want a timeout after 1", id, r, attempts(f))
	}

	// Out of attempts, the last one's result.
	f = filepath.Join(dir, "c")
	out := run(`[` + tries(f, 9) + ` {:timeout 200ms :retry 1 :idempotent #t}]`)
	if r := wait(out); !strings.Contains(r, ":kind :timeout") || attempts(f) != 2 {
		t.Errorf("job %s = %s after %d attempts, want a timeout after 2", out, r, attempts(f))
	}

	// Killed by whiff stop, it runs again at the next start.
	f = filepath.Join(dir, "d")
	stopped := run(`[` + tries(f, 2) + ` {:retry 1 :idempotent #t}]`)
	eventually(t, "the first attempt", func() string { return strconv.Itoa(attempts(f)) }, "1")
	stopDaemon(t, c, done)

	// What a crash leaves: one that may run again, and one whose process
	// may still run, which doesn't.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	running := exec.Command("sleep", "30")
	running.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-running.Process.Pid, syscall.SIGKILL)
		running.Wait()
	})
	f2 := filepath.Join(dir, "e")
	cmd, _ := puff.ReadValue(tries(f2, 1))
	crashed := func(id int64, pid int) *job {
		return &job{ticket: ticket{id: id, user: true, dir: dir}, cmd: cmd.(puff.Vector), pid: pid,
			opts:     jobOpts{timeout: time.Minute, dir: dir, retry: 1, idempotent: true},
			attempts: []attempt{{started: time.Now(), pid: pid}}}
	}
	withDB(t, dir, func(db *bbolt.DB) {
		n, _ := strconv.ParseInt(stopped, 10, 64)
		var j *job
		db.View(func(tx *bbolt.Tx) (err error) {
			j, err = storedJob(n, string(tx.Bucket(tasksBucket).Get(idKey(n))))
			return err
		})
		if j == nil || len(j.attempts) != 1 || j.attempts[0].outcome != "stopped" || j.opts.retry != 1 {
			t.Errorf("job %s is stored as %+v", stopped, j)
		}
		err := db.Update(func(tx *bbolt.Tx) error {
			for _, j := range []*job{crashed(800, gone.Process.Pid), crashed(801, running.Process.Pid)} {
				if err := tx.Bucket(tasksBucket).Put(idKey(j.id), []byte(puff.Print(j.stored()))); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	start(t, dir)
	c = dial(t, dir)
	if r := wait(stopped); r != ok || attempts(f) != 2 {
		t.Errorf("job %s, stopped, = %s after %d attempts, want %s after 2", stopped, r, attempts(f), ok)
	}
	if r := wait("800"); r != ok || attempts(f2) != 1 {
		t.Errorf("job 800, crashed, = %s after %d attempts", r, attempts(f2))
	}
	if r := wait("801"); !strings.Contains(r, ":outcome :unknown :pid") {
		t.Errorf("job 801, which may still run, = %s", r)
	}

	// :jobs/process says which may still run: 801's group, until it's gone.
	process := func(id string) string {
		t.Helper()
		resp, err := c.Eval(`(call :jobs/process `+id+`)`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("process %s: %v %s", id, err, resp.Error)
		}
		return resp.Value
	}
	if got, want := process("801"), fmt.Sprintf("(ok (some %d))", running.Process.Pid); got != want {
		t.Errorf("job 801's process = %s, want %s", got, want)
	}
	syscall.Kill(-running.Process.Pid, syscall.SIGKILL)
	running.Wait()
	for _, id := range []string{"800", "801", "999"} {
		if got := process(id); got != "(ok (none))" {
			t.Errorf("job %s's process = %s, want (ok (none))", id, got)
		}
	}
}
