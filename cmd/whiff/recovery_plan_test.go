package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// planClaude is a claude for a plan: the planner answers two tasks, the
// reviewer approves, and the dev, after a moment, writes a file named after
// its worktree, so a retried dev, which may not hear the task again, writes
// the same one.
const planClaude = `#!/bin/sh
case "$4" in
  Read,Glob,Grep) cat > /dev/null; echo '["one", "two"]' ;;
  "") cat > /dev/null; echo "VERDICT: approve" ;;
  *) cat > /dev/null; sleep 0.1; basename "$PWD" > "$(basename "$PWD").txt"; echo "wrote it" ;;
esac
`

// TestRecoveryPlan is v0.6's acceptance check for a workflow: it kills a
// real daemon at points across a services/plan.puff plan of two tasks, each
// a pipeline run, and starts another. Whatever the point, the plan isn't
// stranded: it was never stored, and starting it again works; or it ends
// done; or it ends failed, saying so in your inbox, and offers what takes
// it on (Plan again, Review again or Retry), which finishes it. Either way
// its last branch has each task once.
func TestRecoveryPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("kills and restarts a daemon at each point")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, after := range []time.Duration{0, 30 * time.Millisecond, 80 * time.Millisecond, 150 * time.Millisecond, 250 * time.Millisecond,
		400 * time.Millisecond, 600 * time.Millisecond, 800 * time.Millisecond, 1000 * time.Millisecond, 1300 * time.Millisecond} {
		t.Run(after.String(), func(t *testing.T) {
			t.Parallel()
			recoverPlanAfter(t, after)
		})
	}
}

var (
	takesOn = regexp.MustCompile(`\[:plan/(replan|review-again|retry) 1\]`)
	doneRun = regexp.MustCompile(`:run (\d+) :step :done`)
)

func recoverPlanAfter(t *testing.T, after time.Duration) {
	h := newPipelineHome(t, planClaude)
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "plan.puff"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "services", "plan.puff"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, c := testDaemon(t, h.dir, h.env...)
	go c.Message("call", "plan/start", `["two files" ["true"]]`, h.repo)
	time.Sleep(after)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, c = testDaemon(t, h.dir, h.env...)

	if plans := h.call(c, "plan/plans", "[]"); plans == "(ok [])" {
		// It died before the plan was stored: its caller had no answer.
		if got := h.call(c, "plan/start", `["two files" ["true"]]`); got != "(ok 1)" {
			t.Fatalf("start again = %s", got)
		}
	}
	var plans string
	for tries := 0; ; tries++ {
		plans = settled(h, c)
		if strings.Contains(plans, ":step :done") {
			break
		}
		if !strings.Contains(plans, ":step :failed") || tries == 3 {
			t.Fatalf("plan 1 ended so, after %d tries: %s", tries, plans)
		}
		if inbox := h.call(c, "me/inbox", "[]"); !strings.Contains(inbox, "plan 1, two files: failed.") {
			t.Fatalf("your inbox = %s", inbox)
		}
		// What takes it on, as it offers it: once the dev whiff died with
		// has ended, if it outlived whiff.
		m := takesOn.FindStringSubmatch(plans)
		if m == nil {
			t.Fatalf("plan 1 offers nothing that takes it on: %s", plans)
		}
		args := "[1]"
		if m[1] == "retry" {
			args = `[1 ""]`
		}
		got := h.call(c, "plan/"+m[1], args)
		for deadline := time.Now().Add(5 * time.Second); strings.Contains(got, ":kind :still-running") && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			got = h.call(c, "plan/"+m[1], args)
		}
		if !strings.HasPrefix(got, "(ok") {
			t.Fatalf("plan/%s = %s", m[1], got)
		}
	}
	// Each task was committed once, the second on the first.
	m := doneRun.FindStringSubmatch(plans)
	if m == nil {
		t.Fatalf("plans = %s", plans)
	}
	if log := h.git("log", "--format=%s", "whiff/"+m[1]); log != "two\none\nfirst\n" {
		t.Errorf("whiff/%s's log = %q", m[1], log)
	}
}

// settled waits for plan 1 to stop planning and building, and returns the
// plans; one that doesn't is stranded.
func settled(h *pipelineHome, c *rpc.Client) string {
	h.t.Helper()
	var plans string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		plans = h.call(c, "plan/plans", "[]")
		if !strings.Contains(plans, ":step :planning") && !strings.Contains(plans, ":step :building") {
			return plans
		}
	}
	h.t.Fatalf("plan 1 is stranded: %s\nruns: %s", plans, h.call(c, "pipeline/runs", "[]"))
	return ""
}
