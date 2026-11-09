package main

import (
	"context"
	"os"
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
	_, err := rpc.DialCommand(context.Background(),
		[]string{"sh", "-c", "echo 'ssh: connect to host vps port 22: Connection refused' >&2; exit 255"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ssh: connect to host vps port 22: Connection refused") {
		t.Errorf("err = %v", err)
	}
	if _, err := rpc.DialCommand(context.Background(), []string{"no-such-command-whiff"}, nil); err == nil {
		t.Error("a command that isn't there connected")
	}
}

// TestPeerProtocol reaches a peer that speaks another protocol: it says
// which, and that the two whiffs need to match.
func TestPeerProtocol(t *testing.T) {
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
