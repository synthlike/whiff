package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// TestMain runs the test binary as a bridge when WHIFF_TEST_BRIDGE names a
// daemon's socket, as whiff bridge does on a peer: stdin and stdout piped
// to it, so a peer here can be another daemon in a temp dir, not ssh.
func TestMain(m *testing.M) {
	if sock := os.Getenv("WHIFF_TEST_BRIDGE"); sock != "" {
		nc, err := net.Dial("unix", sock)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bridge:", err)
			os.Exit(1)
		}
		go func() {
			io.Copy(nc, os.Stdin)
			nc.(*net.UnixConn).CloseWrite()
		}()
		io.Copy(os.Stdout, nc)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestPeers adds peers and follows how each is connected: one reached by
// a bridge to a second daemon connects, goes offline as that daemon stops
// and connects again as it comes back; one whose command fails says why;
// a peer outlives a restart; removing one forgets it.
func TestPeers(t *testing.T) {
	set(t, &peerWait, 20*time.Millisecond)
	set(t, &peerWaitMost, 200*time.Millisecond)
	dir, far := tempDir(t), tempDir(t)
	done := start(t, dir)
	c := dial(t, dir)
	farDone := start(t, far)
	fc := dial(t, far)

	list := func() string {
		t.Helper()
		resp, err := c.Message("call", "peers/list", "[]", "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	waitFor := func(what string, ok func(string) bool) string {
		t.Helper()
		var got string
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if got = list(); ok(got) {
				return got
			}
		}
		t.Fatalf("%s; peers/list = %s", what, got)
		return ""
	}
	call := func(fn, args string) string {
		t.Helper()
		resp, err := c.Message("call", fn, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}

	bridge := fmt.Sprintf(`{:cmd ["env" "WHIFF_TEST_BRIDGE=%s" %q]}`, rpc.SocketPath(far), os.Args[0])
	if got := call("peers/add", `["far" `+bridge+`]`); got != "(ok :done)" {
		t.Fatalf("peers/add = %s", got)
	}
	got := waitFor("far doesn't connect", func(s string) bool { return strings.Contains(s, ":status :connected") })
	if !strings.Contains(got, ":name \"far\"") || !strings.Contains(got, ":seen ") || strings.Contains(got, ":error") {
		t.Errorf("connected, peers/list = %s", got)
	}
	view := call("peers/view", "[]")
	if !strings.Contains(view, `:label "far  connected"`) || !strings.Contains(view, `[:peers/rm "far"]`) {
		t.Errorf("peers/view = %s", view)
	}

	// The far daemon stops: offline, saying so; back: connected again.
	stopDaemon(t, fc, farDone)
	waitFor("far doesn't go offline", func(s string) bool { return strings.Contains(s, ":status :offline") && strings.Contains(s, ":error ") })
	start(t, far)
	waitFor("far doesn't connect again", func(s string) bool { return strings.Contains(s, ":status :connected") })

	// One that can't be reached says what its command said.
	call("peers/add", `["down" {:cmd ["sh" "-c" "echo 'whiff@vps: Permission denied (publickey).' >&2; exit 255"]}]`)
	waitFor("down doesn't say why", func(s string) bool { return strings.Contains(s, "Permission denied (publickey).") })

	for _, bad := range []string{
		`["VPS" {:dest "u@h"}]`, `["vps" {}]`, `["vps" {:dest "u@h" :cmd ["true"]}]`, `["vps" {:dest 7}]`, `["vps" {:cmd []}]`,
	} {
		if got := call("peers/add", bad); !strings.Contains(got, ":bad-peer") {
			t.Errorf("peers/add %s = %s", bad, got)
		}
	}

	// They outlive a restart, and connect again.
	stopDaemon(t, c, done)
	start(t, dir)
	c = dial(t, dir)
	got = waitFor("far doesn't connect after a restart", func(s string) bool { return strings.Contains(s, ":status :connected") })
	if !strings.Contains(got, `"down"`) {
		t.Errorf("after a restart, peers/list = %s", got)
	}

	if got := call("peers/rm", `["down"]`); got != "(ok :done)" {
		t.Errorf("peers/rm = %s", got)
	}
	if got := call("peers/rm", `["down"]`); !strings.Contains(got, ":no-peer") {
		t.Errorf("peers/rm again = %s", got)
	}
	if got := list(); strings.Contains(got, `"down"`) {
		t.Errorf("after peers/rm, peers/list = %s", got)
	}
	// A peer is yours to add: an agent's call is refused.
	ac := dial(t, dir)
	ac.Agent = true
	if resp, _ := ac.Message("call", "peers/add", `["x" {:dest "u@h"}]`, ""); !strings.Contains(resp.Value, ":refused") {
		t.Errorf("an agent's peers/add = %s", resp.Value)
	}
}

func TestPeerArgv(t *testing.T) {
	p := &peer{name: "vps", dest: "whiff@203.0.113.7", whiff: "/home/whiff/go/bin/whiff"}
	got := strings.Join(p.argv(), " ")
	if !strings.HasPrefix(got, "ssh -o BatchMode=yes") || !strings.HasSuffix(got, " whiff@203.0.113.7 /home/whiff/go/bin/whiff bridge") {
		t.Errorf("argv = %s", got)
	}
}
