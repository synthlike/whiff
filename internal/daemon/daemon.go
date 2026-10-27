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
// belongs to the route goroutine, starting and stopping services to the
// supervisor's, and each service's state to its own goroutine.
type daemon struct {
	ctx         context.Context
	ops         chan func(st *state)
	restarts    chan *service // services that faulted, for the supervisor
	log         io.Writer     // notes, and what services print
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

	d := &daemon{ctx: ctx, ops: make(chan func(*state)), restarts: make(chan *service, 16),
		log: os.Stderr, callTimeout: defaultCallTimeout}
	d.base = d.newEnv(io.Discard, &caller{})
	d.wg.Go(d.route)
	jobs := d.jobsService()
	d.do(func(st *state) { st.addrs[jobs.name] = jobs })
	d.wg.Go(func() { jobs.run(ctx) })
	// Clients wait in the listen queue until the services are loaded.
	sv := d.newSupervisor(services)
	sv.scan()
	d.wg.Go(sv.run)
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
// cancels the evaluation it asked for, and lets go of the address it holds.
func (d *daemon) serve(stop func(), closing <-chan struct{}, nc net.Conn) {
	conn := rpc.NewConn(nc)
	l := newLink(conn)
	var held *service // the address the client holds, if any
	defer func() {
		close(l.gone)
		if held != nil {
			d.remove(held)
		}
	}()
	defer func() {
		// When whiff stops, a client sees its connection close only once
		// whiff no longer listens, so it can start the next daemon.
		if d.ctx.Err() != nil {
			<-closing
		}
		conn.Close()
	}()
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
			// An answer goes straight to the delivery waiting for it: the
			// client may be waiting on a request of its own meanwhile.
			if req.Op == "answer" {
				l.answered(req)
				continue
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
		from := ""
		if held != nil {
			from = held.name
		}
		switch req.Op {
		case "hello":
			resp.Version = rpc.Version
		case "eval":
			resp = d.eval(ctx, from, req)
		case "call", "send", "pub":
			resp = d.message(ctx, from, req)
		case "claim":
			if held != nil {
				resp.Error = fmt.Sprintf("this connection holds :%s already", held.name)
				break
			}
			held, resp = d.claim(l, req)
		case "history":
			resp.Out = d.historyText()
		case "ls":
			resp.Out = d.marker().ls()
		case "inbox":
			resp.Out = d.inboxText(strings.TrimPrefix(req.Target, ":"))
		case "reply":
			resp = d.clientReply(req)
		case "wait":
			resp = d.clientWait(ctx, req)
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
// vector, from the client's directory and address (from, "" for none), or
// publishes req.Args's one value to the topic req.Target. Its Value is the
// Result.
func (d *daemon) message(ctx context.Context, from string, req rpc.Request) rpc.Response {
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
		d.publish(message{from: from, topic: topic, args: puff.Print(vec), dir: req.Dir, user: !req.Agent})
		return rpc.Response{Value: puff.Print(puff.Done)}
	}
	vals := []puff.Value{puff.Keyword(strings.TrimPrefix(req.Target, ":"))}
	for i := range vec.Len() {
		vals = append(vals, vec.At(i))
	}
	cur := &caller{addr: from, user: !req.Agent}
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

// eval evaluates req.Src in a fresh environment, in the client's directory,
// acting for its address (from, "" for none).
func (d *daemon) eval(ctx context.Context, from string, req rpc.Request) rpc.Response {
	var out strings.Builder
	env := d.newEnv(&out, &caller{addr: from, user: !req.Agent})
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
