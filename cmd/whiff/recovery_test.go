package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "pipeline.puff"))
	if err != nil {
		t.Fatal(err)
	}
	for _, after := range []time.Duration{0, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		150 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond, 600 * time.Millisecond} {
		t.Run(after.String(), func(t *testing.T) {
			t.Parallel()
			recoverAfter(t, string(src), after)
		})
	}
}

// recoverAfter starts a pipeline run, kills whiff after the given time, and
// checks the run recovers.
func recoverAfter(t *testing.T, src string, after time.Duration) {
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	os.Mkdir(filepath.Join(dir, "services"), 0o700)
	// The dev takes a moment, so whiff can die while it runs; the reviewer
	// approves.
	claude := `#!/bin/sh
cat > /dev/null
if [ "$1 $2 $3 $4" = "-p --strict-mcp-config --tools " ]; then
  echo "looks good"; echo "VERDICT: approve"
  exit 0
fi
sleep 0.1
echo f > feature.txt
echo "changed things"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "pipeline.puff"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	repo := filepath.Join(dir, "repo")
	os.Mkdir(repo, 0o700)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")

	cmd, c := testDaemon(t, dir, env...)
	call := func(c *rpc.Client, fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", fn, args, repo)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	go c.Message("call", "pipeline/start", `["add a feature" ["true"]]`, repo)
	time.Sleep(after)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, c = testDaemon(t, dir, env...)

	n := "1"
	if runs := call(c, "pipeline/runs", "[]"); runs == "(ok [])" {
		// It died before the run was stored: its caller had no answer, and
		// starting it again works: as run 2 if run 1's worktree and branch
		// were made, which stay.
		got := call(c, "pipeline/start", `["add a feature" ["true"]]`)
		if got != "(ok 1)" && got != "(ok 2)" {
			t.Fatalf("start again = %s", got)
		}
		n = got[4:5]
	}
	// step waits for run n to end, and returns how.
	step := func() string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			runs := call(c, "pipeline/runs", "[]")
			for _, s := range []string{":ready", ":failed", ":gave-up", ":cancelled"} {
				if strings.Contains(runs, ":step "+s) {
					return s
				}
			}
		}
		t.Fatalf("run %s is stranded: %s", n, call(c, "pipeline/runs", "[]"))
		return ""
	}
	round := ":round 1"
	switch s := step(); s {
	case ":ready":
	case ":failed":
		// It offers what can be done, and Retry finishes it.
		if inbox := call(c, "me/inbox", "[]"); !strings.Contains(inbox, "pipeline "+n+", add a feature: failed.") || !strings.Contains(inbox, "[:pipeline/retry "+n+"]") {
			t.Fatalf("your inbox = %s", inbox)
		}
		if got := call(c, "pipeline/retry", "["+n+` ""]`); !strings.HasPrefix(got, "(ok") {
			t.Fatalf("retry = %s", got)
		}
		if s := step(); s != ":ready" {
			t.Fatalf("retried, run %s ended %s", n, s)
		}
		round = ":round 2"
	default:
		t.Fatalf("run %s ended %s", n, s)
	}
	// Each result was taken once: one dev round more for a retry, and one
	// commit.
	if runs := call(c, "pipeline/runs", "[]"); !strings.Contains(runs, round) {
		t.Errorf("runs = %s, want %s", runs, round)
	}
	if log := git("log", "--format=%s", "whiff/"+n); log != "add a feature\nfirst\n" {
		t.Errorf("whiff/%s's log = %q", n, log)
	}
}
