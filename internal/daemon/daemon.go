// Package daemon is whiff's daemon: it serves clients on the per-user socket
// and runs the services they talk to.
package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// ErrRunning is returned by Run when another daemon holds dir.
var ErrRunning = errors.New("whiff is already running")

// defaultCallTimeout is how long a call waits for its reply; a var so tests
// can shorten it.
var defaultCallTimeout = 5 * time.Second

// daemon is the state shared by everything Run starts. Routing state
// belongs to the route goroutine; each service's to its own goroutine.
type daemon struct {
	ctx         context.Context
	ops         chan func(addrs map[string]*service)
	log         io.Writer // failed loads, and what services print
	callTimeout time.Duration
	wg          sync.WaitGroup
}

// Run serves clients on the socket in dir until a client stops it or ctx
// ends, with the services in services. A lock file in dir keeps it to one
// daemon per directory.
func Run(ctx context.Context, dir, services string) error {
	if err := rpc.PrepareDir(dir); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "whiff.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrRunning
		}
		return err
	}
	// The lock says no daemon is running, so a socket here is left over.
	path := rpc.SocketPath(dir)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		lock.Close()
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// On stop: stop listening and release the lock before closing
	// connections, so a client that sees its connection close can start the
	// next daemon.
	closing := make(chan struct{})
	go func() {
		<-ctx.Done()
		ln.Close()
		lock.Close()
		close(closing)
	}()

	d := &daemon{ctx: ctx, ops: make(chan func(map[string]*service)), log: os.Stderr, callTimeout: defaultCallTimeout}
	d.wg.Go(d.route)
	// Clients wait in the listen queue until the services are loaded.
	d.loadServices(services)
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				stop()
				d.wg.Wait()
				return err
			}
			break
		}
		d.wg.Go(func() { d.serve(stop, closing, nc) })
	}
	d.wg.Wait()
	return nil
}

// newEnv is a fresh puff env with whiff's functions on top of the base
// table.
func (d *daemon) newEnv(stdout io.Writer) *puff.Env {
	env := puff.NewEnv()
	env.Stdout = stdout
	if err := env.Register(d.builtins()...); err != nil {
		panic(err)
	}
	return env
}

// serve answers one client's requests in order. A client that hangs up
// cancels the evaluation it asked for.
func (d *daemon) serve(stop func(), closing <-chan struct{}, nc net.Conn) {
	conn := rpc.NewConn(nc)
	defer conn.Close()
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	go func() {
		select {
		case <-closing:
			conn.Close()
		case <-ctx.Done():
		}
	}()

	reqs := make(chan rpc.Request)
	go func() {
		defer cancel()
		for {
			var req rpc.Request
			if err := conn.Recv(&req); err != nil {
				return
			}
			select {
			case reqs <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		var req rpc.Request
		select {
		case req = <-reqs:
		case <-ctx.Done():
			return
		}
		var resp rpc.Response
		switch req.Op {
		case "hello":
			resp.Version = rpc.Version
		case "eval":
			resp = d.eval(ctx, req)
		case "stop":
			conn.Send(resp)
			stop()
			continue
		default:
			resp.Error = "unknown op " + req.Op
		}
		if err := conn.Send(resp); err != nil {
			return
		}
	}
}

// eval evaluates req.Src in a fresh environment, in the client's directory.
func (d *daemon) eval(ctx context.Context, req rpc.Request) rpc.Response {
	var out strings.Builder
	env := d.newEnv(&out)
	env.Dir = req.Dir
	v, err := env.Load(ctx, "eval", req.Src)
	resp := rpc.Response{Out: out.String()}
	if err != nil {
		var fault *puff.Fault
		resp.Error = err.Error()
		resp.Fault = errors.As(err, &fault)
		return resp
	}
	if v != nil {
		resp.Value = puff.Print(v)
	}
	return resp
}
