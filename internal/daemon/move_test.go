package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// TestMoveAgent moves scout, an agent on huff, to the peer far and back,
// as whiff move does: pack here, adopt there, drop here. Its
// conversation, spending and mode go with it, this whiff's ids don't; its
// file, a symlink, comes back as one; nothing is deleted. One that isn't
// idle, on Claude, or whose :can isn't there, doesn't move.
func TestMoveAgent(t *testing.T) {
	set(t, &peerWait, 20*time.Millisecond)
	set(t, &peerWaitMost, 200*time.Millisecond)
	dir, far, wd := tempDir(t), tempDir(t), tempDir(t)
	setEnv(t, dir, "HOME", dir)
	setEnv(t, far, "HOME", far)
	notes := `(service notes (on (add x) "Adds." (ok x)))`
	scout := `(agent scout :prompt "Look." :model "local/fake" :can [:notes/add])`
	writeServices(t, dir, map[string]string{"notes": notes, "claudy": `(agent claudy :prompt "Look.")`})
	writeServices(t, far, map[string]string{"notes": notes})
	// scout's file is a symlink into a repository, as installed.
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o700)
	os.WriteFile(filepath.Join(repo, "scout.puff"), []byte(scout), 0o600)
	if err := os.Symlink(filepath.Join(repo, "scout.puff"), filepath.Join(dir, "services", "scout.puff")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, ".whiff", "agents", "scout")
	os.MkdirAll(home, 0o700)
	for name, text := range map[string]string{"session": "s1", "huff-s1.puff": `[{:role :user :text "hi"}]`, "costs": "2026-11-10T10:00:00Z 0.01", "turns": "9"} {
		os.WriteFile(filepath.Join(home, name), []byte(text), 0o600)
	}
	start(t, dir)
	c := dial(t, dir)
	start(t, far)
	fc := dial(t, far)
	call := func(c *rpc.Client, target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	bridge := fmt.Sprintf(`{:cmd ["env" "WHIFF_TEST_BRIDGE=%s" %q] :dir %q}`, rpc.SocketPath(far), os.Args[0], wd)
	if got := call(c, "peers/add", `["far" `+bridge+`]`); got != "(ok :done)" {
		t.Fatalf("peers/add = %s", got)
	}
	eventually(t, "far connects", func() string {
		return fmt.Sprint(strings.Contains(call(c, "peers/list", "[]"), ":status :connected"))
	}, "true")
	if r, err := c.Forward(rpc.ModeRequest("agent/scout", "trusted", []string{"notes/add"}, 0, 5)); err != nil || r.Value != "(ok :done)" {
		t.Fatalf("trust = %+v %v", r, err)
	}

	// Not movable: on Claude, or not there.
	if got := call(c, "agents/pack", `["claudy"]`); !strings.Contains(got, ":not-movable") {
		t.Errorf("pack claudy = %s", got)
	}
	if got := call(c, "agents/pack", `["nobody"]`); !strings.Contains(got, ":no-address") {
		t.Errorf("pack nobody = %s", got)
	}
	// Not idle: a message waits for it, paused; packing leaves it paused.
	os.WriteFile(filepath.Join(home, "paused"), []byte("paused"), 0o600)
	call(c, "scout/tell", `["wait"]`)
	if got := call(c, "agents/pack", `["scout"]`); !strings.Contains(got, ":busy") || !strings.Contains(got, "in its inbox") {
		t.Errorf("pack a busy scout = %s", got)
	}
	inbox, _ := c.Inbox("scout")
	id := strings.Fields(inbox)[0]
	if r, err := c.Message("reply", "", "["+id+" (ok :done)]", ""); err != nil || r.Error != "" {
		t.Fatalf("answer its message: %+v %v", r, err)
	}
	os.WriteFile(filepath.Join(home, "paused"), nil, 0o600)

	// There.
	bundle := call(c, "agents/pack", `["scout"]`)
	if !strings.HasPrefix(bundle, "(ok {") || strings.Contains(bundle, `"turns"`) || !strings.Contains(bundle, `"huff-s1.puff"`) {
		t.Fatalf("pack = %s", bundle)
	}
	v, _ := puff.ReadValue(bundle)
	b := puff.Print(puff.OkValue(v))
	if got := call(c, "agents@far/adopt", "["+b+"]"); got != "(ok :adopted)" {
		t.Fatalf("adopt there = %s", got)
	}
	if got := call(c, "agents/drop", `["scout"]`); got != "(ok :dropped)" {
		t.Fatalf("drop here = %s", got)
	}
	if got := call(c, "scout/usage", "[]"); !strings.Contains(got, ":no-address") {
		t.Errorf("scout here after it moved = %s", got)
	}
	if got := call(c, "scout@far/usage", "[]"); !strings.Contains(got, ":spent 0.01") {
		t.Errorf("scout@far/usage = %s, want its spending with it", got)
	}
	farHome := filepath.Join(far, ".whiff", "agents", "scout")
	for name, want := range map[string]string{"session": "s1", "huff-s1.puff": `[{:role :user :text "hi"}]`, "paused": "", "turns": "(none)"} {
		got, err := os.ReadFile(filepath.Join(farHome, name))
		if err != nil {
			got = []byte("(none)")
		}
		if string(got) != want {
			t.Errorf("there, %s = %q, want %q", name, got, want)
		}
	}
	if r, _ := fc.Eval("(service/status :scout)", ""); !strings.Contains(r.Value, ":mode {:mode :trusted :most 5 :services [:notes/add] :used 0}") || !strings.Contains(r.Value, ":most 5") {
		t.Errorf("its mode there = %s", r.Value)
	}
	if _, err := os.Lstat(filepath.Join(dir, "moved", "scout.puff")); err != nil {
		// the services dir is dir/services: moved is beside it
		t.Errorf("its file here isn't kept in moved: %v", err)
	}

	// Back again, which the laptop drives too.
	bundle = call(c, "agents@far/pack", `["scout"]`)
	if !strings.HasPrefix(bundle, "(ok {") {
		t.Fatalf("pack there = %s", bundle)
	}
	v, _ = puff.ReadValue(bundle)
	if got := call(c, "agents/adopt", "["+puff.Print(puff.OkValue(v))+"]"); got != "(ok :adopted)" {
		t.Fatalf("adopt here = %s", got)
	}
	if got := call(c, "agents@far/drop", `["scout"]`); got != "(ok :dropped)" {
		t.Fatalf("drop there = %s", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "services", "scout.puff")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("its file back here isn't the symlink: %v %v", fi, err)
	}
	if got := call(c, "scout/usage", "[]"); !strings.HasPrefix(got, "(ok {") {
		t.Errorf("scout back here = %s", got)
	}
	aside, _ := filepath.Glob(home + ".moved-*")
	if len(aside) != 1 {
		t.Errorf("its home from before it moved, set aside: %v", aside)
	}

	// One whose :can isn't where it goes doesn't move.
	writeServices(t, far, map[string]string{"notes": `(service notes (on (other) "Else." (ok 1)))`})
	eventually(t, "far's notes changes", func() string {
		return fmt.Sprint(strings.Contains(call(fc, "notes/add", `[1]`), ":no-function"))
	}, "true")
	bundle = call(c, "agents/pack", `["scout"]`)
	v, _ = puff.ReadValue(bundle)
	if got := call(c, "agents@far/adopt", "["+puff.Print(puff.OkValue(v))+"]"); !strings.Contains(got, ":missing") || !strings.Contains(got, ":notes/add") {
		t.Errorf("adopt without its :can there = %s", got)
	}
}
