package rpc

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
