package main

import (
	"fmt"
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

// TestCrash kills a real daemon, as a crash or power cut would, with nothing
// to tidy up: what it stored as it went is all the next one has.
func TestCrash(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	got := filepath.Join(dir, "got")
	services := filepath.Join(dir, "services")
	os.Mkdir(services, 0o700)
	src := `(service waiter
  (on (begin) "Starts a job." (call :jobs/run ["sleep" "30"]))
  (on (pass id) :read "Hands a job on." (hand-on (err {:kind :pending :id id})))
  (on (whiff/done id r) "Notes the answer." (file/append "` + got + `" (string/concat (int/to-string id) " " (value/to-string r) "\n"))))`
	if err := os.WriteFile(filepath.Join(services, "waiter.puff"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := func() (*exec.Cmd, *rpc.Client) {
		t.Helper()
		os.Remove(rpc.SocketPath(dir))
		cmd := exec.Command(os.Args[0], "daemon")
		cmd.Env = append(os.Environ(), "WHIFF_TEST_DIR="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			c, err := rpc.Dial(dir, nil)
			if err == nil {
				t.Cleanup(func() { c.Close() })
				return cmd, c
			}
			if time.Now().After(deadline) {
				t.Fatalf("no daemon: %v", err)
			}
		}
	}
	cmd, c := daemon()
	agent, err := rpc.Dial(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	agent.Agent = true
	pendingID := func(cl *rpc.Client, target, args string) string {
		t.Helper()
		r, err := cl.Message("call", target, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`:id (\d+)`).FindStringSubmatch(r.Value)
		if m == nil {
			t.Fatalf("%s = %s, want it pending", target, r.Value)
		}
		return m[1]
	}
	mine := pendingID(c, "waiter/begin", "[]")
	passed := pendingID(c, "waiter/begin", "[]")
	pendingID(agent, "waiter/pass", "["+passed+"]")
	asked := pendingID(c, "me/ask", `["still there?"]`)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	c.Close()
	_, c = daemon()

	var answers string
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(answers, "\n") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(got)
		answers = string(b)
	}
	// The job's process outlived whiff, which says so and leaves it be.
	m := regexp.MustCompile(`^` + mine + ` \(err \{:kind :interrupted .*:outcome :unknown :pid (\d+)\}\)\n$`).FindStringSubmatch(answers)
	if m == nil {
		t.Fatalf("the job's sender heard %q", answers)
	}
	pid, _ := strconv.Atoi(m[1])
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Errorf("job %s's process: %v", mine, err)
	}
	// The hand-on was stored: the answer goes to whom it was handed, as the
	// agent's.
	r, err := c.Message("wait", "", "["+passed+"]", dir)
	if err != nil || !strings.Contains(r.Value, ":outcome :unknown") {
		t.Errorf("wait for job %s = %+v %v", passed, r, err)
	}
	h, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range h {
		if e.Kind == "done" && strings.HasPrefix(e.Args, "["+passed+" ") {
			found = true
			if e.Origin != "agent" {
				t.Errorf("the answer to job %s is %s's", passed, e.Origin)
			}
		}
	}
	if !found {
		t.Errorf("no answer to job %s in history", passed)
	}
	if text, err := c.Inbox("me"); err != nil || !strings.HasPrefix(text, fmt.Sprintf("%s  client -> :me/ask", asked)) {
		t.Errorf("your inbox = %q %v", text, err)
	}
}
