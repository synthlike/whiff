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
	"sync/atomic"
	"syscall"
	"time"

	"go.etcd.io/bbolt"

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
	resets      chan reset    // whiff reset, for the supervisor
	db          *bbolt.DB     // whiff.db: see store.go
	log         io.Writer     // notes, and what services print
	callTimeout time.Duration
	base        *puff.Env    // only looked up in, for builtins' marks
	tries       atomic.Int64 // the last whiff try's number, for its scratch address
	wg          sync.WaitGroup
}

// Run serves clients on the socket in dir until a client stops it or ctx
// ends, with the services in services and their state in whiff.db next to
// it. A lock file in dir keeps it to one daemon per directory.
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
	// A daemon that is stopping releases the lock before whiff.db, which it
	// holds until its services finish; openStore waits for it.
	db, err := openStore(services)
	if err != nil {
		lock.Close()
		return err
	}
	defer db.Close()
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
		resets: make(chan reset), db: db, log: os.Stderr, callTimeout: defaultCallTimeout}
	d.base = d.newEnv(io.Discard, &caller{})
	d.wg.Go(d.route)
	for _, s := range []*service{d.jobsService(), d.meService()} {
		d.do(func(st *state) { st.addrs[s.name] = s })
		d.wg.Go(func() { s.run(ctx) })
	}
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
	var sub *watcher  // its subscription, if any
	var tr *tracer    // its trace of history, if any
	// The client's REPL keeps its env across requests, acting for cur.
	var repl *puff.Env
	var replOut strings.Builder
	cur := &caller{}
	// cancel stops the evaluation running for the client, if any.
	var cancelEval atomic.Pointer[context.CancelFunc]
	defer func() {
		close(l.gone)
		if held != nil {
			d.remove(held)
		}
		if sub != nil {
			d.unsubscribe(sub)
		}
		if tr != nil {
			d.untrace(tr)
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
			// So does a cancel, which stops the evaluation in progress.
			if req.Op == "cancel" {
				if c := cancelEval.Load(); c != nil {
					(*c)()
				}
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
		case "eval", "repl":
			ectx, cancel := context.WithCancel(ctx)
			cancelEval.Store(&cancel)
			if req.Op == "eval" {
				resp = d.eval(ectx, from, req)
			} else {
				cur.addr, cur.user = from, !req.Agent
				if repl == nil {
					repl = d.newEnv(&replOut, cur)
				}
				resp = d.evalIn(ectx, repl, &replOut, "repl", req)
				replOut.Reset()
			}
			cancelEval.Store(nil)
			cancel()
		case "call", "send", "pub":
			resp = d.message(ctx, from, req)
		case "try":
			resp = d.try(ctx, from, req)
		case "sub":
			if sub != nil {
				resp.Error = "this connection is subscribed already"
				break
			}
			sub, resp = d.subscribe(l, req)
		case "claim":
			if held != nil {
				resp.Error = fmt.Sprintf("this connection holds :%s already", held.name)
				break
			}
			held, resp = d.claim(l, req)
		case "history":
			d.do(func(st *state) { resp.History = st.entries() })
		case "trace":
			if tr != nil {
				resp.Error = "this connection traces history already"
				break
			}
			tr, resp = d.trace(l)
		case "ls":
			resp.Out = d.marker().ls()
		case "inbox":
			resp.Out = d.inboxText(strings.TrimPrefix(req.Target, ":"))
		case "reply":
			resp = d.clientReply(req)
		case "accept", "reject":
			resp = d.clientReview(ctx, req)
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
	return d.evalIn(ctx, env, &out, "eval", req)
}

// evalIn evaluates req.Src, named file in errors, in env, which prints to
// out, in the client's directory.
func (d *daemon) evalIn(ctx context.Context, env *puff.Env, out *strings.Builder, file string, req rpc.Request) rpc.Response {
	env.Dir = req.Dir
	v, err := env.Load(ctx, file, req.Src)
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
