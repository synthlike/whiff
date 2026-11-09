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

// TestFar sends to a peer's addresses, :box@far/fn: a call runs there in
// the peer's dir, or -C's; a task it begins there is one here, answered
// as it ends there, after a restart too, and cancelled there as it is
// here; an agent's message is held by the peer's review gate; an action
// in what comes back is made far; a key runs a message once; a :read
// caller can't reach a :write function.
func TestFar(t *testing.T) {
	set(t, &peerWait, 20*time.Millisecond)
	set(t, &peerWaitMost, 200*time.Millisecond)
	dir, far, wd := tempDir(t), tempDir(t), tempDir(t)
	writeServices(t, far, map[string]string{"box": `(service box
  (on (where) :read "Where it runs." (ok (os/cwd)))
  (on (put x) :write "Keeps x." (ok x))
  (on (view) :read "Its view." (ok {:label "put" :action [:box/put 1]}))
  (ask (q x) "A question."))`})
	writeServices(t, dir, map[string]string{"asker": `(service asker
  (on (go x) "Asks far's box." (call :box@far/q x))
  (on (whiff/done id r) "Its answer." (call :me/tell (str "asker heard " (value/to-string r)))))`})
	done := start(t, dir)
	c := dial(t, dir)
	start(t, far)
	fc := dial(t, far)

	call := func(c *rpc.Client, req rpc.Request) string {
		t.Helper()
		resp, err := c.Forward(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Error != "" {
			return "error: " + resp.Error
		}
		return resp.Value
	}
	here := func(target, args string) string {
		t.Helper()
		return call(c, rpc.Request{Op: "call", Target: target, Args: args})
	}
	// farID is the id of the message waiting in far's inbox addr.
	farID := func(addr string) string {
		t.Helper()
		var text string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if text, _ = fc.Inbox(addr); text != "" {
				return strings.Fields(text)[0]
			}
		}
		t.Fatalf("nothing waits in far's :%s", addr)
		return ""
	}
	connected := func() {
		t.Helper()
		eventually(t, "far connects", func() string {
			return fmt.Sprint(strings.Contains(here("peers/list", "[]"), ":status :connected"))
		}, "true")
	}

	if got := here("box@far/where", "[]"); !strings.Contains(got, ":no-peer") {
		t.Errorf("before far is added, box@far/where = %s", got)
	}
	bridge := fmt.Sprintf(`{:cmd ["env" "WHIFF_TEST_BRIDGE=%s" %q] :dir %q}`, rpc.SocketPath(far), os.Args[0], wd)
	if got := here("peers/add", `["far" `+bridge+`]`); got != "(ok :done)" {
		t.Fatalf("peers/add = %s", got)
	}
	connected()

	// It runs in the peer's dir, or where -C says, never here.
	if got := here("box@far/where", "[]"); got != fmt.Sprintf("(ok %q)", wd) {
		t.Errorf("box@far/where = %s, want the peer's dir", got)
	}
	if got := call(c, rpc.Request{Op: "call", Target: "box@far/where", Args: "[]", Dir: dir, There: "sub"}); got != fmt.Sprintf("(ok %q)", wd+"/sub") {
		t.Errorf("box@far/where -C sub = %s", got)
	}
	if got := call(c, rpc.Request{Op: "call", Target: "box@far/where", Args: "[]", There: "/srv"}); got != `(ok "/srv")` {
		t.Errorf("box@far/where -C /srv = %s", got)
	}
	if got := here("box@far/view", "[]"); !strings.Contains(got, "[:box@far/put 1]") {
		t.Errorf("box@far/view = %s, want its action far", got)
	}
	for target, want := range map[string]string{"box@nope/where": ":no-peer", "box@nope@far/where": "peer's peer", "box@far/nothing": ":no-function"} {
		if got := here(target, "[]"); !strings.Contains(got, want) {
			t.Errorf("%s = %s, want %s", target, got, want)
		}
	}

	// A task there is one here: answered as it ends there.
	id := pendingOf(t, here("box@far/q", `["hi"]`))
	if got := call(fc, rpc.Request{Op: "reply", Args: "[" + farID("box") + " (ok 42)]"}); got != "(ok :done)" {
		t.Fatalf("far's reply = %s", got)
	}
	if got := call(c, rpc.Request{Op: "wait", Args: "[" + id + "]"}); got != "(ok 42)" {
		t.Errorf("wait %s = %s", id, got)
	}
	// A service hears of it as whiff/done.
	here("asker/go", `["why"]`)
	call(fc, rpc.Request{Op: "reply", Args: "[" + farID("box") + " (ok :because)]"})
	eventually(t, "asker hears far's answer", func() string {
		text, _ := c.Inbox("me")
		return fmt.Sprint(strings.Contains(text, `asker heard (ok :because)`))
	}, "true")

	// One that ends while whiff is down is answered as it comes back.
	id = pendingOf(t, here("box@far/q", `["later"]`))
	n := farID("box")
	stopDaemon(t, c, done)
	call(fc, rpc.Request{Op: "reply", Args: "[" + n + " (ok :later)]"})
	start(t, dir)
	c = dial(t, dir)
	if got := call(c, rpc.Request{Op: "wait", Args: "[" + id + "]"}); got != "(ok :later)" {
		t.Errorf("after a restart, wait %s = %s", id, got)
	}
	connected()

	// Cancelled here, it is cancelled there.
	id = pendingOf(t, here("box@far/q", `["never"]`))
	farID("box")
	if got := call(c, rpc.Request{Op: "cancel-task", Args: "[" + id + "]"}); got != "(ok :done)" {
		t.Errorf("cancel %s = %s", id, got)
	}
	eventually(t, "far's ask cancelled", func() string { text, _ := fc.Inbox("box"); return text }, "")

	// An agent's write waits for review there, as a task here.
	reviewer := dial(t, far)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	ac := dial(t, dir)
	ac.Agent = true
	id = pendingOf(t, call(ac, rpc.Request{Op: "call", Target: "box@far/put", Args: "[7]"}))
	held := farID("review")
	if got := call(fc, rpc.Request{Op: "accept", Args: "[" + held + "]"}); got != "(ok 7)" {
		t.Errorf("far's accept = %s", got)
	}
	if got := call(c, rpc.Request{Op: "wait", Args: "[" + id + "]"}); got != "(ok 7)" {
		t.Errorf("the agent's put, wait %s = %s", id, got)
	}
	if got := here("box@far/put", "[8]"); got != "(ok 8)" {
		t.Errorf("your box@far/put = %s", got)
	}

	// A key sent again gets the first's answer, without running again.
	for range 2 {
		if got := call(fc, rpc.Request{Op: "call", Target: "box/put", Args: "[9]", Key: "k1"}); got != "(ok 9)" {
			t.Errorf("box/put with a key = %s", got)
		}
	}
	h, _ := historyOf(fc)
	if n := strings.Count(h, ":box/put 9"); n != 1 {
		t.Errorf("box/put 9 ran %d times:\n%s", n, h)
	}
	// A :read caller's message to a :write function is refused.
	if got := call(fc, rpc.Request{Op: "call", Target: "box/put", Args: "[1]", Read: "a view"}); !strings.Contains(got, ":write") {
		t.Errorf("box/put from a :read caller = %s", got)
	}
}
