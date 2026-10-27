// Package daemon is whiff's daemon: it serves clients on the per-user socket.
package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// ErrRunning is returned by Run when another daemon holds dir.
var ErrRunning = errors.New("whiff is already running")

// Run serves clients on the socket in dir until a client stops it or ctx
// ends. A lock file in dir keeps it to one daemon per directory.
func Run(ctx context.Context, dir string) error {
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

	var wg sync.WaitGroup
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				stop()
				wg.Wait()
				return err
			}
			break
		}
		wg.Go(func() { serve(ctx, stop, closing, nc) })
	}
	wg.Wait()
	return nil
}

// serve answers one client's requests in order. A client that hangs up
// cancels the evaluation it asked for.
func serve(ctx context.Context, stop func(), closing <-chan struct{}, nc net.Conn) {
	conn := rpc.NewConn(nc)
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
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
			resp = eval(ctx, req)
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
func eval(ctx context.Context, req rpc.Request) rpc.Response {
	var out strings.Builder
	env := puff.NewEnv()
	env.Stdout = &out
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
