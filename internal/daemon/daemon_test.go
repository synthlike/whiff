package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
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

// start runs a daemon on dir, with the services in dir/services, and
// returns Run's result. The test's cleanup stops it and waits until it has.
func start(t *testing.T, dir string) <-chan error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- Run(ctx, dir, filepath.Join(dir, "services"))
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return done
}

// set sets *v to x for the test and restores it once the test's daemons
// have stopped, so none reads it as it changes: call it before start.
func set[T any](t *testing.T, v *T, x T) {
	old := *v
	*v = x
	t.Cleanup(func() { *v = old })
}

// dial connects to the daemon on dir, waiting for it to listen.
func dial(t *testing.T, dir string) *rpc.Client {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
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

func wait(t *testing.T, done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("daemon didn't stop")
		return nil
	}
}

func TestEval(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	if c.Daemon != rpc.Version {
		t.Errorf("daemon version %d, want %d", c.Daemon, rpc.Version)
	}
	tests := []struct {
		src  string
		want rpc.Response
	}{
		{"(+ 1 2)", rpc.Response{Value: "3"}},
		{`(io/println "hi") :done`, rpc.Response{Value: ":done", Out: "hi\n"}},
		{"(os/cwd)", rpc.Response{Value: `"/tmp/w"`}},
		{"", rpc.Response{}},
		{"(a", rpc.Response{Error: "eval:1:1: no closing ')'"}},
		{"(+ 1 1.0)", rpc.Response{Error: "eval:1:1: +: can't add int and float", Fault: true}},
		// Each eval starts fresh: x isn't defined any more.
		{"(def x 1)", rpc.Response{}},
		{"x", rpc.Response{Error: "eval:1:1: unknown name x"}},
	}
	for _, tt := range tests {
		got, err := c.Eval(tt.src, "/tmp/w")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if got != tt.want {
			t.Errorf("%s:\ngot  %+v\nwant %+v", tt.src, got, tt.want)
		}
	}
}

func TestOneDaemonPerDir(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	dial(t, dir)
	if err := Run(context.Background(), dir, ""); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run: %v, want ErrRunning", err)
	}
}

func TestStopCancelsEval(t *testing.T) {
	dir := tempDir(t)
	done := start(t, dir)
	busy := dial(t, dir)
	evaled := make(chan struct{})
	go func() {
		busy.Eval("(loop [] (recur))", "")
		close(evaled)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := dial(t, dir).Stop(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Errorf("Run: %v", err)
	}
	if _, err := os.Stat(rpc.SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket after stop: %v", err)
	}
	<-evaled

	// The next daemon starts on the same directory.
	done = start(t, dir)
	if err := dial(t, dir).Stop(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Errorf("second daemon: %v", err)
	}
}
