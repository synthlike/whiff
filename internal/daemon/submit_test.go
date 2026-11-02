package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/rpc"
)

// submitted turns a service's pending into (ok id), the id its whiff/done
// comes back with: for a job that runs, and for one the review gate holds,
// accepted or rejected. A job that can't start stays an err.
func TestSubmitted(t *testing.T) {
	dir := tempDir(t)
	log := filepath.Join(dir, "log")
	touched := filepath.Join(dir, "touched")
	writeServices(t, dir, map[string]string{
		"sub": `(service sub
  (on (go cmd) :read "Submits cmd; claims :read, so an agent's call isn't held." (submitted (call :jobs/run cmd)))
  (on (whiff/done id r) "Logs how it ended."
    (file/append "` + log + `"
      (str id " " (match r [(ok {:exit n}) (str "exit " n)] [(err {:kind k}) (value/to-string k)]) "\n"))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	msg := func(c *rpc.Client, op, target, args string) string {
		t.Helper()
		resp, err := c.Message(op, target, args, "")
		if err != nil {
			t.Fatalf("%s %s: %v", op, target, err)
		}
		return resp.Value
	}
	// go is (ok id), and id is a number.
	submit := func(c *rpc.Client, cmd string) string {
		t.Helper()
		r := msg(c, "call", "sub/go", "["+cmd+"]")
		id, ok := strings.CutPrefix(r, "(ok ")
		if !ok || strings.Trim(id, "0123456789)") != "" {
			t.Fatalf("go %s = %s", cmd, r)
		}
		return strings.TrimSuffix(id, ")")
	}
	logged := func(want string) {
		t.Helper()
		eventually(t, want, func() string { b, _ := os.ReadFile(log); return string(b) }, want)
	}

	// A job that runs: (ok id), then its whiff/done under that id.
	a := submit(c, `["true"]`)
	logged(a + " exit 0\n")

	// One that can't start is the err, and nothing comes later.
	if r := msg(c, "call", "sub/go", `[["/no/such/program"]]`); !strings.Contains(r, ":kind :start") {
		t.Errorf("go of a missing program = %s", r)
	}

	// An agent's chain: the gate holds the job, and go is (ok id) all the
	// same. Accepting runs it under that id, and its result is the one
	// whiff/done; rejecting answers that id with :rejected.
	b := submit(agent, `["touch" "`+touched+`"]`)
	if _, err := os.Stat(touched); err == nil {
		t.Fatal("the held job ran before the review")
	}
	if r := msg(c, "accept", "", "["+b+"]"); !strings.Contains(r, ":id "+b+" :kind :pending") {
		t.Errorf("accept = %s, want the job's pending, under the same id", r)
	}
	logged(a + " exit 0\n" + b + " exit 0\n")
	if _, err := os.Stat(touched); err != nil {
		t.Errorf("the accepted job didn't run: %v", err)
	}
	d := submit(agent, `["touch" "`+touched+`"]`)
	if r := msg(c, "reject", "", "["+d+` "no"]`); r != "(ok :done)" {
		t.Errorf("reject = %s", r)
	}
	logged(a + " exit 0\n" + b + " exit 0\n" + d + " :rejected\n")
}
