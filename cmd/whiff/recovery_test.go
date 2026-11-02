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

// TestRecovery is v0.6's acceptance check. It kills a real daemon at points
// across a services/pipeline.puff run, from its submission to its last
// result, and starts another. Whatever the point, nothing is stranded: the
// run was never stored, and starting it again works; or it ends ready; or
// it ends failed and offers Retry, which finishes it. Either way each result
// is taken once.
func TestRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("kills and restarts a daemon at each point")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, after := range []time.Duration{0, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		150 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond, 600 * time.Millisecond} {
		t.Run(after.String(), func(t *testing.T) {
			t.Parallel()
			recoverAfter(t, after)
		})
	}
}

// fakeClaude is a claude for the pipeline: the reviewer approves, and the
// dev, after slow, a shell command, makes a change.
func fakeClaude(slow string) string {
	return `#!/bin/sh
cat > /dev/null
if [ "$1 $2 $3 $4" = "-p --strict-mcp-config --tools " ]; then
  echo "looks good"; echo "VERDICT: approve"
  exit 0
fi
` + slow + `
echo f > feature.txt
echo "changed things"
`
}

// pipelineHome is a whiff home with services/pipeline.puff, a fake claude
// and a git repo with one commit, for a test to run whiff in.
type pipelineHome struct {
	t         *testing.T
	dir, repo string
	env       []string
}

func newPipelineHome(t *testing.T, claude string) *pipelineHome {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	os.Mkdir(filepath.Join(dir, "services"), 0o700)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "pipeline.puff"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &pipelineHome{t: t, dir: dir, repo: filepath.Join(dir, "repo"),
		env: []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}}
	os.Mkdir(h.repo, 0o700)
	h.git("init", "-q", "-b", "main")
	h.git("commit", "-q", "--allow-empty", "-m", "first")
	return h
}

func (h *pipelineHome) git(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = h.repo, append(os.Environ(), h.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (h *pipelineHome) call(c *rpc.Client, fn, args string) string {
	h.t.Helper()
	resp, err := c.Message("call", fn, args, h.repo)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Value
}

// ended waits for run n, the only one, to end, and returns how.
func (h *pipelineHome) ended(c *rpc.Client, n string) string {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		runs := h.call(c, "pipeline/runs", "[]")
		for _, s := range []string{":ready", ":failed", ":gave-up", ":cancelled"} {
			if strings.Contains(runs, ":step "+s) {
				return s
			}
		}
	}
	h.t.Fatalf("run %s is stranded: %s", n, h.call(c, "pipeline/runs", "[]"))
	return ""
}

// committed checks run n's branch has the one commit it should.
func (h *pipelineHome) committed(n string) {
	h.t.Helper()
	if log := h.git("log", "--format=%s", "whiff/"+n); log != "add a feature\nfirst\n" {
		h.t.Errorf("whiff/%s's log = %q", n, log)
	}
}

// recoverAfter starts a pipeline run, kills whiff after the given time, and
// checks the run recovers.
func recoverAfter(t *testing.T, after time.Duration) {
	// The dev takes a moment, so whiff can die while it runs.
	h := newPipelineHome(t, fakeClaude("sleep 0.1"))
	cmd, c := testDaemon(t, h.dir, h.env...)
	go c.Message("call", "pipeline/start", `["add a feature" ["true"]]`, h.repo)
	time.Sleep(after)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, c = testDaemon(t, h.dir, h.env...)

	n := "1"
	if runs := h.call(c, "pipeline/runs", "[]"); runs == "(ok [])" {
		// It died before the run was stored: its caller had no answer, and
		// starting it again works: as run 2 if run 1's worktree and branch
		// were made, which stay.
		got := h.call(c, "pipeline/start", `["add a feature" ["true"]]`)
		if got != "(ok 1)" && got != "(ok 2)" {
			t.Fatalf("start again = %s", got)
		}
		n = got[4:5]
	}
	round := ":round 1"
	switch s := h.ended(c, n); s {
	case ":ready":
	case ":failed":
		// It offers what can be done, and Retry finishes it: once the dev
		// whiff died with has ended, if it outlived whiff.
		if inbox := h.call(c, "me/inbox", "[]"); !strings.Contains(inbox, "pipeline "+n+", add a feature: failed.") || !strings.Contains(inbox, "[:pipeline/retry "+n+"]") {
			t.Fatalf("your inbox = %s", inbox)
		}
		got := h.call(c, "pipeline/retry", "["+n+` ""]`)
		for deadline := time.Now().Add(5 * time.Second); strings.HasPrefix(got, "(err {:kind :still-running") && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			got = h.call(c, "pipeline/retry", "["+n+` ""]`)
		}
		if !strings.HasPrefix(got, "(ok") {
			t.Fatalf("retry = %s", got)
		}
		if s := h.ended(c, n); s != ":ready" {
			t.Fatalf("retried, run %s ended %s", n, s)
		}
		round = ":round 2"
	default:
		t.Fatalf("run %s ended %s", n, s)
	}
	// Each result was taken once: one dev round more for a retry, and one
	// commit.
	if runs := h.call(c, "pipeline/runs", "[]"); !strings.Contains(runs, round) {
		t.Errorf("runs = %s, want %s", runs, round)
	}
	h.committed(n)
}

// TestRecoveryStray kills whiff while the dev runs, and the dev outlives it:
// its write's outcome is unknown, and it may still be making it. The run
// fails saying so, and Retry, which would start a second dev in the same
// worktree, refuses until the first is stopped.
func TestRecoveryStray(t *testing.T) {
	if testing.Short() {
		t.Skip("kills and restarts a daemon")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// The first dev writes its pid, which leads its process group, and
	// sleeps; a retried one doesn't.
	h := newPipelineHome(t, fakeClaude(`if [ ! -e "$HOME/dev.pid" ]; then echo $$ > "$HOME/dev.pid"; sleep 60; fi`))
	cmd, c := testDaemon(t, h.dir, h.env...)
	if got := h.call(c, "pipeline/start", `["add a feature" ["true"]]`); got != "(ok 1)" {
		t.Fatalf("start = %s", got)
	}
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the dev didn't start")
		}
		b, _ := os.ReadFile(filepath.Join(h.dir, "dev.pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, c = testDaemon(t, h.dir, h.env...)

	if s := h.ended(c, "1"); s != ":failed" {
		t.Fatalf("run 1 ended %s", s)
	}
	group := strconv.Itoa(pid)
	if inbox := h.call(c, "me/inbox", "[]"); !strings.Contains(inbox, "may still be running, as process group "+group) || !strings.Contains(inbox, "kill -- -"+group) {
		t.Fatalf("your inbox = %s", inbox)
	}
	if got := h.call(c, "pipeline/retry", `[1 ""]`); !regexp.MustCompile(`^\(err \{:kind :still-running .*:pid ` + group + `\}\)$`).MatchString(got) {
		t.Fatalf("retry while the dev runs = %s", got)
	}
	// You stop it, and Retry finishes the run.
	syscall.Kill(-pid, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d is still there", pid)
		}
	}
	if got := h.call(c, "pipeline/retry", `[1 ""]`); !strings.HasPrefix(got, "(ok") {
		t.Fatalf("retry once it stopped = %s", got)
	}
	if s := h.ended(c, "1"); s != ":ready" {
		t.Fatalf("retried, run 1 ended %s", s)
	}
	h.committed("1")
}
