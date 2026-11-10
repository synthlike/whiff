package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/rpc"
)

// whiff cancel ends a task that hasn't ended, however it waits: held,
// waiting for a reply or running. An agent cancels only its own session's.
func TestCancel(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	file := filepath.Join(dir, "touched")
	writeServices(t, dir, map[string]string{"box": `(service box (ask (q x) "A question."))`})
	start(t, dir)
	you := dial(t, dir)
	agent, other := dial(t, dir), dial(t, dir)
	for _, a := range []*rpc.Client{agent, other} {
		a.Agent = true
		if _, err := a.Claim("agent", ""); err != nil {
			t.Fatal(err)
		}
	}
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		r, err := c.Message(op, target, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		if r.Error != "" {
			return "error: " + r.Error
		}
		return r.Value
	}
	cancel := func(c *rpc.Client, id string) string { return msg(c, "cancel-task", "", "["+id+"]") }
	wait := func(id string) string { return msg(you, "wait", "", "["+id+"]") }
	notStarted := func(id, why string) string {
		return `(err {:kind :cancelled :message "task ` + id + ` was cancelled before ` + why + `" :outcome :not-started})`
	}

	// One waiting for a reply leaves its inbox; only you cancel yours.
	q := pendingOf(t, msg(you, "call", "box/q", `["x"]`))
	if r := cancel(agent, q); !strings.Contains(r, ":kind :refused") {
		t.Errorf("an agent's cancel of yours = %s", r)
	}
	if r := cancel(you, q); r != "(ok :done)" {
		t.Errorf("cancel %s = %s", q, r)
	}
	if r := wait(q); r != notStarted(q, ":box answered it") {
		t.Errorf("wait %s = %s", q, r)
	}
	if text, _ := you.Inbox("box"); text != "" {
		t.Errorf("inbox of :box = %q", text)
	}

	// One held leaves the review queue, and nothing runs; another session
	// can't cancel it.
	held := pendingOf(t, msg(agent, "call", "jobs/run", `[["touch" "`+file+`"]]`))
	if r := cancel(other, held); !strings.Contains(r, ":kind :refused") {
		t.Errorf("another session's cancel = %s", r)
	}
	if r := cancel(agent, held); r != "(ok :done)" {
		t.Errorf("cancel %s = %s", held, r)
	}
	if r := wait(held); r != notStarted(held, "it started") {
		t.Errorf("wait %s = %s", held, r)
	}
	if text, _ := you.Inbox("review"); text != "" {
		t.Errorf("the review queue = %q", text)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("the cancelled job ran")
	}

	// A running job is killed; cancelled again, it has ended.
	job := pendingOf(t, msg(you, "call", "jobs/run", `[["sleep" "30"]]`))
	if r := cancel(you, job); r != "(ok :done)" {
		t.Errorf("cancel %s = %s", job, r)
	}
	stopped := `(err {:kind :cancelled :message "job ` + job + ` was cancelled" :outcome :stopped :stderr "" :stdout ""})`
	if r := wait(job); r != stopped {
		t.Errorf("wait %s = %s", job, r)
	}
	if r := cancel(you, job); r != `(err {:kind :ended :message "task `+job+` has ended" :result `+stopped+`})` {
		t.Errorf("cancel %s, ended = %s", job, r)
	}
	if r := cancel(you, "999"); !strings.Contains(r, ":kind :no-id") {
		t.Errorf("cancel 999 = %s", r)
	}
}

// TestSessionEndsWithJob has a service's job hold a new session, as a
// pipeline dev does, which asks another service something: once the job
// ends, its ask, which nobody would hear the answer to, is cancelled, and
// its sender hears so.
func TestSessionEndsWithJob(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"boss":   `(service boss (on (hire) "Runs a worker." (call :jobs/run ["sh" "-c" "echo $WHIFF_SESSION > token; while [ ! -e done ]; do sleep 0.02; done"] {:session [:helper/ask]})))`,
		"helper": `(service helper (ask (ask q) "Asks the helper."))`,
	})
	start(t, dir)
	c := dial(t, dir)
	if r, _ := c.Eval(`(call :boss/hire)`, dir); !strings.HasPrefix(r.Value, "(err {:id ") {
		t.Fatalf("hire = %+v", r)
	}
	var tok string
	eventually(t, "the worker's token", func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "token"))
		tok = strings.TrimSpace(string(b))
		return fmt.Sprint(len(tok))
	}, "32")
	worker := dial(t, dir)
	worker.Agent = true
	if r, err := worker.ClaimSession(tok, ""); err != nil || !strings.HasPrefix(r.Value, "(ok [:agent/") {
		t.Fatalf("the worker's claim = %+v %v", r, err)
	}
	r, _ := worker.Message("call", "helper/ask", `["which port?"]`, "")
	q := strings.Fields(strings.TrimPrefix(r.Value, "(err {:id "))[0]
	if text, _ := c.Inbox("helper"); !strings.Contains(text, "which port?") {
		t.Fatalf("helper's inbox = %q", text)
	}
	os.WriteFile(filepath.Join(dir, "done"), nil, 0o600)
	eventually(t, "helper's inbox, once the worker's job ends", func() string { text, _ := c.Inbox("helper"); return text }, "")
	if t.Failed() {
		return // the wait below would wait for ever
	}
	if r, err := c.Message("wait", "", "["+q+"]", ""); err != nil || !strings.Contains(r.Value, ":kind :cancelled") {
		t.Errorf("the worker's ask = %+v %v, want it cancelled", r, err)
	}
}

// A connection holding a session is an agent's though a later request
// leaves the flag out: its write is held for review, and it can't cancel
// your task.
func TestSessionStaysAgent(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	file := filepath.Join(dir, "touched")
	writeServices(t, dir, map[string]string{"box": `(service box (ask (q x) "A question."))`})
	start(t, dir)
	you, agent := dial(t, dir), dial(t, dir)
	agent.Agent = true
	if _, err := agent.Claim("agent", ""); err != nil {
		t.Fatal(err)
	}
	agent.Agent = false
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		r, err := c.Message(op, target, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		if r.Error != "" {
			return "error: " + r.Error
		}
		return r.Value
	}
	pendingOf(t, msg(agent, "call", "jobs/run", `[["touch" "`+file+`"]]`))
	if _, err := os.Stat(file); err == nil {
		t.Error("the session's write ran ungated")
	}
	q := pendingOf(t, msg(you, "call", "box/q", `["x"]`))
	if r := msg(agent, "cancel-task", "", "["+q+"]"); !strings.Contains(r, ":kind :refused") {
		t.Errorf("the session's cancel of yours = %s", r)
	}
}
