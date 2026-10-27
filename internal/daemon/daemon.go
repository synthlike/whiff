// Package daemon is whiff's daemon: it serves clients on the per-user socket
// and runs the services they talk to.
package daemon

import (
	"context"
	"errors"
	"fmt"
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
	ops         chan func(st *state)
	log         io.Writer // notes, and what services print
	callTimeout time.Duration
	base        *puff.Env // only looked up in, for builtins' marks
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

	d := &daemon{ctx: ctx, ops: make(chan func(*state)), log: os.Stderr, callTimeout: defaultCallTimeout}
	d.base = d.newEnv(io.Discard, &caller{})
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

// newEnv is a fresh puff env with whiff's functions, acting for cur, on top
// of the base table.
func (d *daemon) newEnv(stdout io.Writer, cur *caller) *puff.Env {
	env := puff.NewEnv()
	env.Stdout = stdout
	if err := env.Register(d.builtins(cur)...); err != nil {
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
		case "call", "send", "pub":
			resp = d.message(ctx, req)
		case "history":
			resp.Out = d.historyText()
		case "ls":
			resp.Out = d.marker().ls()
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

// message calls or sends req.Target, :address/fn, with req.Args, a printed
// vector, from the client's directory, or publishes req.Args's one value to
// the topic req.Target. Its Value is the Result.
func (d *daemon) message(ctx context.Context, req rpc.Request) rpc.Response {
	args, err := puff.ReadValue(req.Args)
	vec, ok := args.(puff.Vector)
	if err != nil || !ok {
		return rpc.Response{Error: fmt.Sprintf("want arguments as a vector, got %s", req.Args)}
	}
	if req.Op == "pub" {
		topic := strings.TrimPrefix(req.Target, ":")
		if topic == "" || vec.Len() != 1 {
			return rpc.Response{Error: "pub wants a topic and one value"}
		}
		d.publish(message{topic: topic, args: puff.Print(vec), dir: req.Dir})
		return rpc.Response{Value: puff.Print(puff.Done)}
	}
	vals := []puff.Value{puff.Keyword(strings.TrimPrefix(req.Target, ":"))}
	for i := range vec.Len() {
		vals = append(vals, vec.At(i))
	}
	cur := &caller{}
	m, addr, err := cur.message(req.Dir, vals)
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	var r puff.Value
	if req.Op == "send" {
		if r = d.deliver(addr, m); r == nil {
			r = puff.Done
		}
	} else if r, err = d.request(ctx, cur, addr, m); err != nil {
		return rpc.Response{Error: err.Error()}
	}
	return rpc.Response{Value: puff.Print(r)}
}

// eval evaluates req.Src in a fresh environment, in the client's directory.
func (d *daemon) eval(ctx context.Context, req rpc.Request) rpc.Response {
	var out strings.Builder
	env := d.newEnv(&out, &caller{})
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
