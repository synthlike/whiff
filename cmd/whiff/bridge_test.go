package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// TestBridge reaches a daemon through whiff bridge, as a peer does over
// ssh: the bridge starts the daemon there, calls go through as the user's,
// and closing the connection ends the bridge, leaving the daemon running.
func TestBridge(t *testing.T) {
	t.Parallel()
	// A short directory: a unix socket path must fit in about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "whiff-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	services := filepath.Join(dir, "services")
	os.Mkdir(services, 0o700)
	os.WriteFile(filepath.Join(services, "greet.puff"), []byte(`(service greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name))))
`), 0o600)
	t.Cleanup(func() {
		if c, err := rpc.Dial(dir, nil); err == nil {
			c.Stop()
			c.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bridge := []string{os.Args[0], "bridge"}
	env := []string{"WHIFF_TEST_DIR=" + dir}

	for round := range 2 {
		c, err := rpc.DialCommand(ctx, bridge, env)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := c.PeerCompatible("peer"); err != nil {
			t.Error(err)
		}
		// The daemon loads its services as it starts.
		var resp rpc.Response
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if resp, err = c.Message("call", "greet/hello", `["peer"]`, ""); err != nil || !strings.Contains(resp.Value, "no-address") || time.Now().After(deadline) {
				break
			}
		}
		if err != nil || resp.Value != `(ok "hello, peer")` {
			t.Errorf("round %d: call through the bridge = %s %v", round, resp.Value, err)
		}
		began := time.Now()
		c.Close()
		if took := time.Since(began); took > time.Second {
			t.Errorf("round %d: the bridge took %v to end once closed", round, took)
		}
	}
	// It was the user's call, in the daemon's own history.
	c, err := rpc.Dial(dir, nil)
	if err != nil {
		t.Fatalf("the daemon the bridge started isn't running: %v", err)
	}
	defer c.Close()
	es, _ := c.History()
	if !strings.Contains(rpc.HistoryText(es), `call  user   client -> :greet/hello "peer"`) {
		t.Errorf("history:\n%s", rpc.HistoryText(es))
	}
}

// TestBridgeFails reaches a peer by a command that ends without answering,
// as ssh does when it can't log in: the error says what it printed.
func TestBridgeFails(t *testing.T) {
	// Not in parallel: it lowers rpc.HelloWait for every client.
	_, err := rpc.DialCommand(context.Background(),
		[]string{"sh", "-c", "echo 'ssh: connect to host vps port 22: Connection refused' >&2; exit 255"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ssh: connect to host vps port 22: Connection refused") {
		t.Errorf("err = %v", err)
	}
	if _, err := rpc.DialCommand(context.Background(), []string{"no-such-command-whiff"}, nil); err == nil {
		t.Error("a command that isn't there connected")
	}
	// One that waits, as ssh for an agent's approval, is given up on.
	was := rpc.HelloWait
	rpc.HelloWait = 100 * time.Millisecond
	defer func() { rpc.HelloWait = was }()
	began := time.Now()
	_, err = rpc.DialCommand(context.Background(), []string{"sleep", "30"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no answer within 100ms") || time.Since(began) > 5*time.Second {
		t.Errorf("a command that never answers: %v after %v", err, time.Since(began))
	}
}

// TestPeerProtocol reaches a peer that speaks another protocol: it says
// which, and that the two whiffs need to match.
func TestPeerProtocol(t *testing.T) {
	t.Parallel()
	old := strconv.Itoa(rpc.Version - 1)
	c, err := rpc.DialCommand(context.Background(),
		[]string{"sh", "-c", `read hello; echo '{"version":` + old + `}'; cat >/dev/null`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	want := "vps speaks protocol " + old + ", this whiff " + strconv.Itoa(rpc.Version) + ": install the same whiff on both"
	if err := c.PeerCompatible("vps"); err == nil || err.Error() != want {
		t.Errorf("PeerCompatible = %v; want %s", err, want)
	}
}

// TestPeerQueues lists a peer's review queue and inbox beside this
// whiff's, each id with the peer's name, and accepts, replies and cancels
// there by it; a peer that isn't connected says so.
func TestPeerQueues(t *testing.T) {
	t.Parallel()
	daemon := func(files map[string]string) string {
		dir, err := os.MkdirTemp("/tmp", "whiff-q-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		os.Mkdir(filepath.Join(dir, "services"), 0o700)
		for name, src := range files {
			os.WriteFile(filepath.Join(dir, "services", name+".puff"), []byte(src), 0o600)
		}
		cmd := exec.Command(os.Args[0], "daemon")
		cmd.Env = append(os.Environ(), "WHIFF_TEST_DIR="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if c, err := rpc.Dial(dir, nil); err == nil {
				c.Stop()
				c.Close()
			}
			cmd.Wait()
		})
		return dir
	}
	dial := func(dir string) *rpc.Client {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			c, err := rpc.Dial(dir, nil)
			if err == nil {
				t.Cleanup(func() { c.Close() })
				return c
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
		}
	}
	here, far := daemon(nil), daemon(map[string]string{"box": `(service box (on (put x) :write "Keeps x." (ok x)))`})
	c, fc := dial(here), dial(far)
	whiff := func(args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(args, here, filepath.Join(here, "services"), &out, &errs)
		return code, out.String(), errs.String()
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}

	bridge := fmt.Sprintf(`["far" {:cmd ["env" "WHIFF_TEST_DIR=%s" %q "bridge"]}]`, far, os.Args[0])
	if resp, err := c.Message("call", "peers/add", bridge, ""); err != nil || resp.Value != "(ok :done)" {
		t.Fatalf("peers/add = %s %v", resp.Value, err)
	}
	eventually("far doesn't connect", func() bool {
		resp, _ := c.Message("call", "peers/list", "[]", "")
		return strings.Contains(resp.Value, ":status :connected")
	})

	// An agent's write held there, and a question to you there.
	reviewer := dial(far)
	reviewer.Sub("review/pending")
	agent := dial(far)
	agent.Agent = true
	held, _ := agent.Message("call", "box/put", "[7]", "")
	asked, _ := fc.Message("call", "me/ask", `["go?"]`, "")
	idOf := func(r string) string { return strings.Fields(strings.TrimPrefix(r, "(err {:id "))[0] }
	heldID, askedID := idOf(held.Value), idOf(asked.Value)

	if _, out, _ := whiff("pending"); !strings.Contains(out, heldID+"@far  client -> :box/put 7") {
		t.Errorf("whiff pending:\n%s", out)
	}
	if code, out, errs := whiff("accept", heldID+"@far"); code != 0 || out != "7\n" {
		t.Errorf("whiff accept %s@far = %d %q %q", heldID, code, out, errs)
	}
	if _, out, _ := whiff("inbox"); !strings.Contains(out, askedID+"@far  client -> :me/ask \"go?\"") {
		t.Errorf("whiff inbox:\n%s", out)
	}
	if code, _, errs := whiff("reply", askedID+"@far", "yes"); code != 0 {
		t.Errorf("whiff reply %s@far = %d %s", askedID, code, errs)
	}
	if r, _ := fc.Message("wait", "", "["+askedID+"]", ""); r.Value != `(ok "yes")` {
		t.Errorf("far's wait %s = %s", askedID, r.Value)
	}
	if _, out, _ := whiff("inbox", "me@far"); out != "" {
		t.Errorf("whiff inbox me@far, all answered:\n%s", out)
	}
	if code, _, _ := whiff("wait", askedID+"@far"); code != 2 {
		t.Errorf("whiff wait %s@far = %d, want 2", askedID, code)
	}
	if code, _, errs := whiff("accept", "1@nope"); code != 1 || !strings.Contains(errs, "no peer nope") {
		t.Errorf("whiff accept 1@nope = %d %s", code, errs)
	}

	fc.Stop()
	eventually("far doesn't go", func() bool {
		_, out, _ := whiff("pending")
		return strings.Contains(out, "peer far is offline: its inbox isn't shown") || strings.Contains(out, "peer far is connecting")
	})
}
