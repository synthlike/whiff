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
	services    string        // the services directory
	callTimeout time.Duration
	base        *puff.Env    // only looked up in, for builtins' marks
	tries       atomic.Int64 // the last whiff try's number, for its scratch address
	agents      atomic.Int64 // the last agent's number, for its :agent/<n>
	timers      atomic.Int64 // the last timer's number: see keepTimer
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
		resets: make(chan reset), db: db, log: os.Stderr, services: services, callTimeout: defaultCallTimeout}
	d.base = d.newEnv(io.Discard, &caller{})
	d.timers.Store(time.Now().UnixNano())
	err = db.View(func(tx *bbolt.Tx) error {
		n, err := storedInt(tx, agentsKey)
		d.agents.Store(n)
		return err
	})
	if err != nil {
		lock.Close()
		return err
	}
	d.wg.Go(d.route)
	for _, s := range []*service{d.jobsService(), d.meService(), d.reviewService()} {
		d.do(func(st *state) { st.addrs[s.name] = s })
		d.wg.Go(func() { s.run(ctx) })
	}
	// Clients wait in the listen queue until the services are loaded.
	sv := d.newSupervisor(services)
	sv.scan()
	d.resume()
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
			// A free session's code runs as if you had accepted it.
			free := false
			if req.Agent && from != "" {
				d.do(func(st *state) { free = st.lets(from, "") })
			}
			if req.Op == "eval" {
				resp = d.eval(ectx, from, free, req)
			} else {
				cur.addr, cur.user, cur.reviewed, cur.session = from, !req.Agent, free, session(from, req)
				if repl == nil {
					repl = d.newEnv(&replOut, cur)
				}
				resp = d.evalIn(ectx, repl, cur, &replOut, "repl", req)
				replOut.Reset()
			}
			cancelEval.Store(nil)
			cancel()
		case "call", "send", "pub":
			resp = d.message(ctx, from, req)
		case "try":
			resp = d.try(ctx, from, req)
		case "test":
			resp = d.test(ctx, req)
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
			resp = d.clientReply(from, req)
		case "mode":
			resp = d.clientMode(req)
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
		r := d.publishGated(message{from: from, topic: topic, args: puff.Print(vec), dir: req.Dir, user: !req.Agent, session: session(from, req)})
		return rpc.Response{Value: puff.Print(r)}
	}
	vals := []puff.Value{puff.Keyword(strings.TrimPrefix(req.Target, ":"))}
	for i := range vec.Len() {
		vals = append(vals, vec.At(i))
	}
	cur := &caller{addr: from, user: !req.Agent, session: session(from, req)}
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
	} else if t, ok := r.(puff.Tag); ok && t.Name() == "ok" && cur.user {
		d.actedOn(addr, m, puff.NewVector(vals...))
	}
	return rpc.Response{Value: puff.Print(r)}
}

// eval evaluates req.Src in a fresh environment, in the client's directory,
// acting for its address (from, "" for none); as accepted if free.
func (d *daemon) eval(ctx context.Context, from string, free bool, req rpc.Request) rpc.Response {
	var out strings.Builder
	cur := &caller{addr: from, user: !req.Agent, reviewed: free, session: session(from, req)}
	return d.evalIn(ctx, d.newEnv(&out, cur), cur, &out, "eval", req)
}

// session is the agent session a client's request begins chains in: its
// address, from, if it is an agent's; "" for anyone else.
func session(from string, req rpc.Request) string {
	if !req.Agent {
		return ""
	}
	return from
}

// evalIn evaluates req.Src, named file in errors, in env, which prints to
// out and acts for cur, in the client's directory. An agent's code that
// reaches :write waits for review instead, as a whole; the rest runs under
// :read. A free session's, cur.reviewed, runs as it is.
func (d *daemon) evalIn(ctx context.Context, env *puff.Env, cur *caller, out *strings.Builder, file string, req rpc.Request) rpc.Response {
	from, free := cur.addr, cur.reviewed
	env.Dir = req.Dir
	// A free session's eval that writes goes in history, as it runs, and
	// the messages it sends are its.
	freeWrite := false
	if req.Agent {
		if forms, err := puff.Read(file, req.Src); err == nil {
			write := d.evalMark(env, from, forms) == puff.MarkWrite
			switch {
			case free:
				freeWrite = write
			case write:
				var r puff.Value
				d.do(func(st *state) { r = d.holdEval(st, from, req) })
				return rpc.Response{Value: puff.Print(r)}
			default:
				ctx = puff.WithMark(ctx, "the agent's eval", puff.MarkRead)
			}
		}
	}
	if freeWrite {
		d.do(func(st *state) { cur.id = st.msgID() })
		defer func() { cur.id = 0 }()
	}
	begin := time.Now()
	v, err := env.Load(ctx, file, req.Src)
	if freeWrite {
		reply := "(ok :done)"
		switch {
		case err != nil:
			reply = puff.Print(puff.Errorf("fault", "%v", err))
		case v != nil:
			reply = puff.Print(v)
		}
		d.do(func(st *state) {
			st.record(entry{at: begin, kind: "eval", id: cur.id, from: from, args: puff.Print(puff.NewVector(puff.String(req.Src))),
				reply: reply, took: time.Since(begin), gate: "free"})
		})
	}
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
