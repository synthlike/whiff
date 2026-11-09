// Package daemon is whiff's daemon: it serves clients on the per-user socket
// and runs the services they talk to.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
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
	dir         string        // the socket's directory, which a session job's client dials: WHIFF_DIR
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
		resets: make(chan reset), db: db, log: os.Stderr, services: services, dir: dir, callTimeout: defaultCallTimeout}
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
	// whiff's own addresses: see own.
	for _, s := range []*service{d.jobsService(), d.meService(), d.reviewService(), d.tasksService(), d.timersService(), d.peersService()} {
		d.do(func(st *state) { st.addrs[s.name] = s })
		d.wg.Go(func() { s.run(ctx) })
	}
	// Clients wait in the listen queue until the services are loaded.
	sv := d.newSupervisor(services)
	sv.scan()
	d.resume()
	d.wg.Go(sv.run)
	d.startPeers()
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

	// queued is a request on its way to the loop below; an eval's has its
	// context already, so a cancel read after it stops it even if it
	// hasn't started yet.
	type queued struct {
		req    rpc.Request
		ctx    context.Context
		cancel *context.CancelFunc
	}
	reqs := make(chan queued)
	go func() {
		defer cancel()
		for {
			var q queued
			req := &q.req
			if err := conn.Recv(req); err != nil {
				return
			}
			// An answer goes straight to the delivery waiting for it: the
			// client may be waiting on a request of its own meanwhile.
			if req.Op == "answer" {
				l.answered(*req)
				continue
			}
			// So does a cancel, which stops the evaluation in progress.
			if req.Op == "cancel" {
				if c := cancelEval.Load(); c != nil {
					(*c)()
				}
				continue
			}
			if req.Op == "eval" || req.Op == "repl" {
				var c context.CancelFunc
				q.ctx, c = context.WithCancel(ctx)
				q.cancel = &c
				cancelEval.Store(q.cancel)
			}
			select {
			case reqs <- q:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		var q queued
		select {
		case q = <-reqs:
		case <-ctx.Done():
			return
		}
		req := q.req
		var resp rpc.Response
		from := ""
		if held != nil {
			from = held.name
		}
		if held != nil && held.can != nil {
			if why := fenced(held, req); why != "" {
				if err := conn.Send(rpc.Response{Error: why}); err != nil {
					return
				}
				continue
			}
		}
		switch req.Op {
		case "hello":
			resp.Version = rpc.Version
		case "eval", "repl":
			ectx := q.ctx
			// A free session's code runs as if you had accepted it.
			free := false
			if req.Agent && from != "" {
				d.do(func(st *state) { free = st.lets(from, "") })
			}
			if req.Op == "eval" {
				resp = d.eval(ectx, from, free, req)
			} else {
				c := d.clientCaller(from, req, free)
				cur.addr, cur.id, cur.lineage = c.addr, c.id, c.lineage
				if repl == nil {
					repl = d.newEnv(&replOut, cur)
				}
				resp = d.evalIn(ectx, repl, cur, &replOut, "repl", req)
				replOut.Reset()
			}
			// Unless a later eval has come in meanwhile.
			cancelEval.CompareAndSwap(q.cancel, nil)
			(*q.cancel)()
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
			if req.All {
				var err error
				if resp.History, err = d.allHistory(); err != nil {
					resp.Error = err.Error()
				}
				break
			}
			d.do(func(st *state) { resp.History = st.entries() })
		case "trace":
			if tr != nil {
				resp.Error = "this connection traces history already"
				break
			}
			tr, resp = d.trace(l, req)
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
		case "cancel-task":
			resp = d.clientCancel(from, req)
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

// fenced is why s, an agent turn's session, may not make req: it may call
// or send only to its :can functions, read its own inbox, answer it, and
// wait on or cancel tasks; "" if it may. Its only evals are the reads
// whiff mcp lists its tools with, exactly: (session/mode), and (address/
// describe :addr) for an address in :can.
func fenced(s *service, req rpc.Request) string {
	switch req.Op {
	case "hello", "wait", "reply", "cancel-task":
		return ""
	case "eval":
		if req.Src == "(session/mode)" {
			return ""
		}
		for fn := range s.can {
			addr := fn[:strings.LastIndex(fn, "/")]
			if req.Src == "(address/describe :"+addr+")" {
				return ""
			}
		}
	case "inbox":
		if strings.TrimPrefix(req.Target, ":") == s.name {
			return ""
		}
		return ":" + s.name + " reads only its own inbox"
	case "call", "send":
		if s.can[strings.TrimPrefix(req.Target, ":")] {
			return ""
		}
		return fmt.Sprintf(":%s may call only %s, its agent's :can, not %s", s.name, canList(s.can), req.Target)
	}
	return fmt.Sprintf(":%s may only call %s, its agent's :can: no %s", s.name, canList(s.can), req.Op)
}

// canList is can's functions, as puff keywords in order.
func canList(can map[string]bool) string {
	fns := slices.Sorted(maps.Keys(can))
	for i, fn := range fns {
		fns[i] = ":" + fn
	}
	if len(fns) == 0 {
		return "nothing"
	}
	return strings.Join(fns, " ")
}

// message calls or sends req.Target, :address/fn, with req.Args, a printed
// vector, from the client's directory and address (from, "" for none), or
// publishes req.Args's one value to the topic req.Target. Its Value is the
// Result.
func (d *daemon) message(ctx context.Context, from string, req rpc.Request) rpc.Response {
	if key := req.Key; key != "" && req.Op != "pub" {
		req.Key = ""
		return d.once(key, func() rpc.Response { return d.message(ctx, from, req) })
	}
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
		cur := d.clientCaller(from, req, false)
		r := d.publishGated(message{cause: cur.id, from: from, topic: topic, args: puff.Print(vec), dir: req.Dir, lineage: cur.lineage})
		return rpc.Response{Value: puff.Print(r)}
	}
	vals := []puff.Value{puff.Keyword(strings.TrimPrefix(req.Target, ":"))}
	for i := range vec.Len() {
		vals = append(vals, vec.At(i))
	}
	cur := d.clientCaller(from, req, false)
	m, addr, err := cur.message(req.Dir, vals)
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	if _, _, far := farAddr(addr); far {
		m.there, m.read = req.There, req.Read
	} else {
		if req.There != "" {
			m.dir = filepath.Join(req.Dir, req.There)
			if filepath.IsAbs(req.There) {
				m.dir = req.There
			}
		}
		// A peer's view calls a :read function, as it would here.
		if req.Read != "" {
			if err := d.readOnly(puff.WithMark(ctx, req.Read, puff.MarkRead), addr, m); err != nil {
				return rpc.Response{Error: err.Error()}
			}
		}
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
	cur := d.clientCaller(from, req, free)
	return d.evalIn(ctx, d.newEnv(&out, cur), cur, &out, "eval", req)
}

// clientLineage is the chain a client's request begins: the user's, or
// for an agent's, in its session, its address from; as accepted if free,
// a free session's.
func (d *daemon) clientLineage(from string, req rpc.Request, free bool) lineage {
	if !req.Agent {
		return lineage{user: true, reviewed: free}
	}
	l := lineage{session: from, reviewed: free}
	d.do(func(st *state) {
		l.task = st.sessionJob(from)
		if j := st.jobs[l.task]; j != nil {
			l.project = j.project
		}
	})
	// A session of its own, as Claude Code's in a repository, works in
	// the project it was started in.
	if l.project == "" && l.task == 0 {
		l.project = projectOf(req.Dir)
	}
	return l
}

// clientCaller is who a client's request acts as: from, in the chain
// clientLineage gives; for an agent's session, its messages are caused by
// the message that began the job holding it, as the jobs/run of an
// agent's turn, so history shows them in the chain they are work for.
func (d *daemon) clientCaller(from string, req rpc.Request, free bool) *caller {
	cur := &caller{addr: from, lineage: d.clientLineage(from, req, free)}
	if req.Agent {
		d.do(func(st *state) { cur.id = st.sessionCause(from) })
	}
	return cur
}

// sessionCause is the message that began the job holding session, the
// cause of what the session sends; 0 if none holds it.
func (st *state) sessionCause(session string) int64 {
	if j := st.jobs[st.sessionJob(session)]; j != nil {
		return j.msg
	}
	return 0
}

// sessionJob is the job holding session, agent/<n> or an agent's own,
// such as the turn it runs in: the parent of the tasks the session
// begins. 0 if none holds it, as an agent's own between turns.
func (st *state) sessionJob(session string) int64 {
	if session == "" {
		return 0
	}
	for _, g := range st.grants {
		if g.session == session || slices.Contains(g.claimed, session) {
			return g.job
		}
	}
	return 0
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
			// What a review shows is the eval's own code, and a held one
			// runs later: a used file could have changed by then.
			for _, f := range forms {
				if isList(f, "use") {
					return rpc.Response{Error: f.Pos.String() + ": an agent's eval can't use files; a service can"}
				}
			}
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
	cause := cur.id // what began the session's job, if any
	if freeWrite {
		d.do(func(st *state) { cur.id = st.msgID() })
		defer func() { cur.id = cause }()
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
			st.record(entry{at: begin, kind: "eval", id: cur.id, cause: cause, project: cur.project, from: from, args: puff.Print(puff.NewVector(puff.String(req.Src))),
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

// own says whether name is one of whiff's own addresses, which no service
// file or client may take.
func own(name string) bool {
	return name == "jobs" || name == "me" || name == "review" || name == "tasks" || name == "peers"
}
