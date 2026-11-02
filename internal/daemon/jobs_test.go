package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestJobStop checks that a stopped job's group gets SIGTERM, then SIGKILL
// for what ignores it once stopGrace is over: on cancel and on whiff stop.
func TestJobStop(t *testing.T) {
	set(t, &stopGrace, 300*time.Millisecond)
	dir := tempDir(t)
	termed := filepath.Join(dir, "termed")
	writeServices(t, dir, map[string]string{})
	done := start(t, dir)
	c := dial(t, dir)
	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	// run runs script and waits until it says it's ready; it returns the
	// job's id and its process group.
	run := func(script string) (string, int) {
		t.Helper()
		id := eval(`(match (call :jobs/run ["sh" "-c" "` + script + `"]) [(err {:kind :pending :id id}) id])`)
		eventually(t, "ready", func() string { return eval(`(call :jobs/output ` + id + `)`) }, `(ok {:stderr "" :stdout "ready\n"})`)
		pid, _ := strconv.Atoi(eval(`(match (call :jobs/process ` + id + `) [(ok (some p)) p])`))
		return id, pid
	}
	cancelled := func(id string) {
		t.Helper()
		want := `done  user   :jobs -> client ` + id + ` (err {:kind :cancelled`
		eventually(t, "cancelled", func() string {
			h, _ := historyOf(c)
			if strings.Contains(h, want) {
				return want
			}
			return "not in history"
		}, want)
	}
	gone := func(pid int) {
		t.Helper()
		eventually(t, "group gone", func() string {
			return strconv.FormatBool(errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH))
		}, "true")
	}

	// SIGTERM first, so a server can shut down cleanly.
	id, _ := run(`trap 'echo bye > ` + termed + `; exit 0' TERM; echo ready; sleep 10 & wait`)
	eval(`(call :jobs/cancel ` + id + `)`)
	cancelled(id)
	if b, _ := os.ReadFile(termed); string(b) != "bye\n" {
		t.Errorf("the job's trap wrote %q, want it to have run on SIGTERM", b)
	}

	// A group that ignores SIGTERM gets SIGKILL once the grace is over.
	id, pid := run(`trap '' TERM; echo ready; sleep 10`)
	began := time.Now()
	eval(`(call :jobs/cancel ` + id + `)`)
	cancelled(id)
	if took := time.Since(began); took < stopGrace {
		t.Errorf("cancelled after %v, before the %v grace was over", took, stopGrace)
	}
	gone(pid)

	// whiff stop stops its jobs the same way, and waits for them.
	_, pid = run(`trap '' TERM; echo ready; sleep 10`)
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	gone(pid)
}

func TestJobs(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	if err := os.WriteFile(filepath.Join(dir, "here"), []byte("in dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeServices(t, dir, map[string]string{
		"builder": `(service builder
  (on (build) "Runs a job in the caller's directory." (call :jobs/run ["cat" "here"]))
  (on (via) "Gets a job from hander." (call :hander/cat "handed on"))
  (on (whiff/done id r) "Writes the job's output."
    (match r [(ok {:stdout out}) (file/write "` + got + `" out)])))`,
		"watch": `(service watch
  (on :jobs/done (v) (ok v)))`,
		"hander": `(service hander
  (on (cat text) "Hands its caller a job." (hand-on (call :jobs/run ["cat"] {:stdin text})))
  (on (late) "Hands on a job that is done already, twice."
    (let [r (call :jobs/run ["echo" "late"])]
      (do (loop []
            (match (call :jobs/list)
              [(ok []) (ok :done)]
              [_ (do (proc/run ["sleep" "0.01"]) (recur))]))
          (match (hand-on r) [(err {:kind :pending}) (hand-on r)]))))
  (on (whiff/done id r) "Writes the answers it got." (file/write "` + filepath.Join(dir, "hander-got") + `" (value/to-string r)))
  (on (pass x) "Hands on an ask." (hand-on (call :box/q x)))
  (on (bogus) "Hands on what isn't its own." (hand-on (err {:kind :pending :id 999})))
  (on (plain) "Hands on a value." (hand-on (ok 1))))`,
		"box":  `(service box (ask (q x) "Waits."))`,
		"jobs": `(service jobs)`,
	})
	start(t, dir)
	c := dial(t, dir)

	// run evaluates src, a call that should be pending, and returns its id.
	run := func(src string) string {
		t.Helper()
		resp, err := c.Eval(`(match `+src+` [(err {:kind :pending :id id}) id])`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	inHistory := func(want string) {
		t.Helper()
		eventually(t, want, func() string {
			h, err := historyOf(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(h, want) {
				return want
			}
			return "not in history"
		}, want)
	}

	id := run(`(call :jobs/run ["sh" "-c" "echo hi; echo oops >&2; exit 3"])`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 3 :stderr "oops\n" :stdout "hi\n"})`)
	inHistory(`pub   user   :jobs -> :watch on :jobs/done {:cmd ["sh" "-c" "echo hi; echo oops >&2; exit 3"] :cwd "` + dir + `" :id ` + id)

	run(`(call :builder/build)`)
	eventually(t, "builder's answer", func() string { b, _ := os.ReadFile(got); return string(b) }, "in dir")

	id = run(`(call :jobs/run ["cat"] {:stdin "piped"})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 0 :stderr "" :stdout "piped"})`)
	id = run(`(call :jobs/run ["echo" "model"] {:as :text :origin :agent})`)
	inHistory(`done  agent  :jobs -> client ` + id + ` (ok "model\n")`)
	id = run(`(call :jobs/run ["pwd"] {:dir "/" :as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok "/\n")`)
	id = run(`(call :jobs/run ["cat"] {:stdin "as text" :as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok "as text")`)
	id = run(`(call :jobs/run ["sh" "-c" "echo no >&2; exit 2"] {:as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:exit 2 :kind :exit :message "job ` + id + ` exited 2" :stderr "no\n"})`)

	// hand-on hands the caller a job or an ask: to a client, to a service's
	// whiff/done, and when the answer came before the hand-on.
	id = run(`(call :hander/cat "to you")`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 0 :stderr "" :stdout "to you"})`)
	run(`(call :builder/via)`)
	eventually(t, "the job hander handed on", func() string { b, _ := os.ReadFile(got); return string(b) }, "handed on")
	// The answer came first, so both hander and its caller get it; the
	// second hand-on finds nothing left to hand on.
	resp, err := c.Eval(`(call :hander/late)`, dir)
	if err != nil || !strings.HasPrefix(resp.Value, `(err {:kind :no-id :message "no job or ask `) {
		t.Errorf("handing on twice = %s %v", resp.Value, err)
	}
	inHistory(` (ok {:exit 0 :stderr "" :stdout "late\n"})`)
	// hander's copy is history only once hander has handled it.
	inHistory(`:jobs -> :hander/whiff/done `)
	h, _ := historyOf(c)
	if !strings.Contains(h, `  done  user   :jobs -> client `) {
		t.Errorf("want the late answer to go to both the client and hander:\n%s", h)
	}
	id = run(`(call :hander/pass 5)`)
	if _, err := c.Message("reply", "", "["+id+" (ok 6)]", ""); err != nil {
		t.Fatal(err)
	}
	inHistory(`done  user   client -> client ` + id + ` (ok 6)`)
	for src, want := range map[string]string{
		`(call :hander/bogus)`: `(err {:kind :no-id :message "no job or ask 999 of :hander's to hand on"})`,
		`(call :hander/plain)`: `(ok 1)`,
	} {
		if resp, err := c.Eval(src, dir); err != nil || resp.Value != want {
			t.Errorf("%s = %s %s %v, want %s", src, resp.Value, resp.Error, err, want)
		}
	}
	// A send has nobody waiting for its answer, so there is nobody to hand on to.
	c.Eval(`(send :hander/cat "x")`, dir)
	inHistory(`send  user   client -> :hander/cat "x"  (err {:kind :no-id :message "only a call's handler can hand on: nobody waits for this message's answer"})`)

	id = run(`(call :jobs/run ["sleep" "10"])`)
	resp, err = c.Eval(`(call :jobs/list)`, dir)
	if want := `(ok [{:cmd ["sleep" "10"] :cwd "` + dir + `" :id ` + id + `}])`; err != nil || resp.Value != want {
		t.Errorf("list = %s %v, want %s", resp.Value, err, want)
	}
	resp, err = c.Eval(`(call :jobs/cancel `+id+`)`, dir)
	if err != nil || resp.Value != "(ok :done)" {
		t.Errorf("cancel = %s %v", resp.Value, err)
	}
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:kind :cancelled :message "job ` + id + ` was cancelled" :outcome :stopped})`)

	// :jobs/output is a running job's output so far; once it ends, its result.
	id = run(`(call :jobs/run ["sh" "-c" "echo up; echo warn >&2; sleep 10"])`)
	output := func() string {
		resp, err := c.Eval(`(call :jobs/output `+id+`)`, dir)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	eventually(t, "output so far", output, `(ok {:stderr "warn\n" :stdout "up\n"})`)
	c.Eval(`(call :jobs/cancel `+id+`)`, dir)
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:kind :cancelled`)
	if got, want := output(), `(err {:kind :no-id :message "no job `+id+` is running: once it ends, whiff wait `+id+` has its result"})`; got != want {
		t.Errorf("output after the end = %s, want %s", got, want)
	}

	id = run(`(call :jobs/run ["sleep" "10"] {:timeout 50ms})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:kind :timeout :message "job ` + id + ` took longer than 50ms" :outcome :stopped})`)

	tests := []struct{ src, want string }{
		{`(call :jobs/run [])`, `(err {:kind :bad-call :message ":jobs/run: want a command like [\"go\" \"test\"], got []"})`},
		{`(call :jobs/run ["true"] {:wait 1s})`, `(err {:kind :bad-call :message ":jobs/run: want {:timeout d :stdin s :dir path :as :text :origin :agent :retry n :idempotent b}, a positive duration and count and strings, got {:wait 1s}"})`},
		{`(call :jobs/run ["true"] {:as :json})`, `(err {:kind :bad-call :message ":jobs/run: want {:timeout d :stdin s :dir path :as :text :origin :agent :retry n :idempotent b}, a positive duration and count and strings, got {:as :json}"})`},
		{`(call :jobs/run)`, `(err {:kind :bad-call :message ":jobs/run takes 1 to 2 arguments, got 0"})`},
		{`(match (call :jobs/run ["no-such-program-whiff"]) [(err {:kind k}) k])`, `:start`},
		{`(call :jobs/cancel 999)`, `(err {:kind :no-id :message "no job 999 is running"})`},
	}
	for _, tt := range tests {
		resp, err := c.Eval(tt.src, dir)
		if err != nil || resp.Value != tt.want {
			t.Errorf("%s = %s %s %v, want %s", tt.src, resp.Value, resp.Error, err, tt.want)
		}
	}

	if resp, err := c.Eval(`(hand-on 1)`, dir); err != nil || !resp.Fault || resp.Error != "eval:1:1: hand-on: only a service's handler can hand on" {
		t.Errorf("hand-on from a client = %+v %v", resp, err)
	}

	// A service file can't take :jobs's address.
	inHistory(`jobs.puff:1:1: :jobs is whiff's own address; rename the file`)
}
