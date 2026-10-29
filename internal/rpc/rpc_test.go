package rpc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChains(t *testing.T) {
	at := time.Date(2026, 10, 29, 10, 42, 1, 0, time.Local)
	ms := func(n int) time.Time { return at.Add(time.Duration(n) * time.Millisecond) }
	// As history records them: a handler's messages before its own entry.
	es := []Entry{
		{At: ms(0), Kind: "note", Note: "started"},
		{At: ms(2), Kind: "call", ID: 2, Cause: 1, Origin: "user", From: "plan", To: "git/status", Args: "[]", Reply: `(ok "clean")`},
		{At: ms(1), Kind: "pub", ID: 3, Cause: 1, Origin: "user", From: "plan", To: "log on :plan/drafted", Args: `["x"]`, Reply: "(ok :done)"},
		{At: ms(0), Kind: "call", ID: 1, Origin: "user", To: "plan/draft", Args: `["x"]`, Reply: "(ok :done)"},
		{At: ms(3), Kind: "call", ID: 5, Cause: 2, Origin: "user", From: "git", To: "fs/read", Args: `["x"]`, Reply: `(ok "")`},
		{At: ms(5), Kind: "call", ID: 4, Cause: 9, Origin: "agent", From: "agent/3", To: "todo/add", Args: `["y"]`, Reply: "(ok 1)"},
	}
	var got []string
	for _, c := range Chains(es) {
		got = append(got, fmt.Sprintf("%d:%d", c.ID, c.Depth))
	}
	// 4's cause isn't in es; a note has no id.
	if want := "4:0 1:0 3:1 2:1 5:2 0:0"; strings.Join(got, " ") != want {
		t.Errorf("chains: %s, want %s", strings.Join(got, " "), want)
	}
	if text, want := ChainsText(es), "10:42:01.003      call  user   :git -> :fs/read \"x\"  (ok \"\")\n"; !strings.Contains(text, want) {
		t.Errorf("chains text:\n%s\nwant a line\n%s", text, want)
	}
}

// tempDir is a short directory under /tmp: a unix socket path must fit in
// about 100 bytes, and t.TempDir's may not.
func tempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestPrepareDir(t *testing.T) {
	base := tempDir(t)

	fresh := filepath.Join(base, "fresh")
	if err := PrepareDir(fresh); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o700 {
		t.Errorf("fresh: mode %v, want 0700", fi.Mode().Perm())
	}
	if err := PrepareDir(fresh); err != nil {
		t.Errorf("existing: %v", err)
	}

	open := filepath.Join(base, "open")
	os.Mkdir(open, 0o700)
	os.Chmod(open, 0o755)
	if err := PrepareDir(open); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Errorf("open: %v, want open to other users", err)
	}

	link := filepath.Join(base, "link")
	os.Symlink(fresh, link)
	if err := PrepareDir(link); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("link: %v, want not a directory", err)
	}
}

// fakeDaemon answers every request on dir's socket with version v.
func fakeDaemon(t *testing.T, dir string, v int) {
	ln, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			c := NewConn(nc)
			var req Request
			for c.Recv(&req) == nil {
				c.Send(Response{Version: v})
			}
			c.Close()
		}
	}()
}

func TestCompatible(t *testing.T) {
	tests := []struct {
		version int
		want    string
	}{
		{Version, ""},
		{Version - 1, "run `whiff stop`"},
		{Version + 1, "reinstall whiff"},
	}
	for _, tt := range tests {
		dir := tempDir(t)
		fakeDaemon(t, dir, tt.version)
		c, err := Dial(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = c.Compatible()
		c.Close()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("version %d: %v", tt.version, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("version %d: %v, want %q", tt.version, err, tt.want)
		}
	}
}

func TestDialStarts(t *testing.T) {
	dir := tempDir(t)
	started := false
	c, err := Dial(dir, func() error {
		started = true
		fakeDaemon(t, dir, Version)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !started {
		t.Error("start wasn't called")
	}
}
