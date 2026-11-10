package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMove runs whiff move between two real daemons, the second a peer of
// the first, reached by this binary as its bridge: scout goes there and
// comes back, and a move it can't make says why, leaving it where it was.
func TestMove(t *testing.T) {
	mk := func() string {
		dir, err := os.MkdirTemp("/tmp", "whiff-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		os.Mkdir(filepath.Join(dir, "services"), 0o700)
		os.WriteFile(filepath.Join(dir, "services", "notes.puff"), []byte(`(service notes (on (add x) "Adds." (ok x)))`), 0o600)
		return dir
	}
	here, far := mk(), mk()
	os.WriteFile(filepath.Join(here, "services", "scout.puff"), []byte(`(agent scout :prompt "Look." :model "local/fake" :can [:notes/add])`), 0o600)
	_, c := testDaemon(t, here, "HOME="+here)
	testDaemon(t, far, "HOME="+far)
	whiff := func(args ...string) (string, string, int) {
		var out, errs strings.Builder
		code := run(args, here, filepath.Join(here, "services"), &out, &errs)
		return out.String(), errs.String(), code
	}
	bridge := fmt.Sprintf(`{:cmd ["env" "WHIFF_TEST_DIR=%s" %q "bridge"]}`, far, os.Args[0])
	if r, err := c.Message("call", "peers/add", `["far" `+bridge+`]`, ""); err != nil || r.Value != "(ok :done)" {
		t.Fatalf("peers/add = %+v %v", r, err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if r, _ := c.Message("call", "peers/list", "[]", ""); strings.Contains(r.Value, ":connected") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("far didn't connect")
		}
	}

	if _, errs, code := whiff("move", "scout", "here"); code != 2 || !strings.Contains(errs, "somewhere else") {
		t.Errorf("move scout here = %d %s", code, errs)
	}
	if out, errs, code := whiff("move", "scout", "far"); code != 0 || out != "scout moved far: it is :scout@far now\n" {
		t.Fatalf("move scout far = %d %q %s", code, out, errs)
	}
	if out, _, _ := whiff("call", "scout@far/usage"); !strings.Contains(out, ":turns 0") {
		t.Errorf("scout@far/usage = %s", out)
	}
	// It can't come back while there's an agent of its name here.
	os.WriteFile(filepath.Join(here, "services", "scout.puff"), []byte(`(agent scout :prompt "Another." :model "local/fake")`), 0o600)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if out, _, _ := whiff("call", "scout/usage"); strings.HasPrefix(out, "{") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other scout didn't load")
		}
	}
	if _, errs, code := whiff("move", "scout@far", "here"); code != 1 || !strings.Contains(errs, "adopt scout here; it stays far: there is an address :scout here already") {
		t.Errorf("move back over another = %d %s", code, errs)
	}
	if b, err := os.ReadFile(filepath.Join(far, ".whiff", "agents", "scout", "paused")); err != nil || len(b) != 0 {
		t.Errorf("after the failed move, it is paused there: %q %v", b, err)
	}
	os.Remove(filepath.Join(here, "services", "scout.puff"))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if out, _, _ := whiff("call", "scout/usage"); !strings.HasPrefix(out, "{") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other scout didn't go")
		}
	}
	if out, errs, code := whiff("move", "scout@far", "here"); code != 0 || out != "scout moved here: it is :scout now\n" {
		t.Fatalf("move scout@far here = %d %q %s", code, out, errs)
	}
}
