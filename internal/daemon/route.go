package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// mailboxSize bounds each address's mailbox.
const mailboxSize = 100

// message asks an address to run one of its functions, or its subscriber to
// a topic. The arguments are a printed puff vector, so nothing but data
// travels.
type message struct {
	// ctx is the caller's, done when it stops waiting; nil for a send or a
	// publish, which the service bounds itself.
	ctx   context.Context
	from  string // the sender's address; "" for a client
	fn    string
	topic string // instead of fn, for a published value
	args  string
	dir   string // the caller's working directory
	// chain is the addresses waiting on calls that led to this one, so a
	// call back into one of them fails at once instead of timing out.
	chain []string
	// user is whether the chain m belongs to began with the user; one
	// without a clear origin is the agent's.
	user bool
	// reviewed is whether you accepted m, or a message earlier in its
	// chain, at the review gate, which lets it through.
	reviewed bool
	// session is the agent session, agent/<n>, whose message began m's
	// chain; "" for a chain that began elsewhere.
	session string
	// gate is how the review gate let an agent's write through, for
	// history: trusted, free, accepted <id>, or for <session> further
	// down a chain it let through; "" for anything else.
	gate string
	// reply gets the printed Result, nil for a send. It has room for it, so
	// the service never waits on a caller that has gone.
	reply chan string
	// id is given as m is delivered, one per delivery of a publish, so
	// its entry in history has its own; cause is the id of what led to
	// it: see rpc.Entry.
	id, cause int64
	// kept is where m stays stored until its service has handled it: the
	// result of a task, for a service that handles whiff/done, or a timer.
	kept kept
	// held is the id m waited under in :review's inbox, for a message you
	// accepted: a job it starts keeps it, so its sender has one id.
	held int64
}

// origin is :user or :agent, the origin of the chain a message belongs to.
func origin(user bool) puff.Keyword {
	if user {
		return "user"
	}
	return "agent"
}

// kind is "call", "send", "pub" or "done", for a [:whiff/done id result].
func (m message) kind() string {
	switch {
	case m.topic != "":
		return "pub"
	case m.fn == "whiff/done":
		return "done"
	case m.reply != nil:
		return "call"
	}
	return "send"
}

// to is what m is for at addr: addr/fn, or addr on :topic.
func (m message) to(addr string) string {
	if m.topic != "" {
		return addr + " on :" + m.topic
	}
	return addr + "/" + m.fn
}

// caller is who whiff's builtins act for: a service handling a message, or
// a client, which has no address.
type caller struct {
	addr  string
	id    int64    // the message being handled, or the eval running, that the messages sent are caused by; 0 for none
	chain []string // addresses waiting in the current chain, addr last
	user  bool     // whether the current chain began with the user
	// reviewed is whether you accepted a message of the current chain at
	// the review gate.
	reviewed bool
	session  string // the agent session that began the current chain, if one did
	// stop is closed when the service version acting stops; nil for a
	// client. placed is closed once that version has its address.
	stop, placed <-chan struct{}
	// handOn gives the sender of the message being handled the answer to
	// a job or ask; nil outside a handler.
	handOn func(id int64) error
}

// state is what the route goroutine owns.
type state struct {
	db       *bbolt.DB // whiff.db: see store.go
	log      io.Writer
	addrs    map[string]*service
	history  []entry         // the last historySize entries, oldest first
	unstored []entry         // history the writer hasn't taken: see record
	dropped  int             // entries left out of unstored since it was last taken
	kick     chan<- struct{} // wakes the history writer
	asked    []*asked        // every service's inbox, oldest first
	timers   []*timer        // what load read, until resume takes it up
	lost     []*job          // what load read, until resume takes it up
	owed     []answered      // results for services that aren't loaded, until they are
	cut      []ticket        // what load read, until resume takes it up: accepted as whiff went
	jobs     map[int64]*job
	nextID   int64                       // the last id given to a message answered later, or a job
	lastMsg  int64                       // the last id given to a message or an eval in history: see msgID
	waiters  map[int64][]chan puff.Value // whiff waits, by the id they wait for
	subs     map[*watcher]bool           // clients' subscriptions: whiff sub
	tracers  map[*tracer]bool            // clients following history: whiff history -f
	modes    map[string]*mode            // agent sessions that aren't gated, by address
	grants   map[string]grant            // agent turns' session tokens: see grant
}

// grant is what an agent turn's session token lets its whiff mcp claim:
// the session, :agent/<the agent's name>, and the functions it may call.
// whiff makes one as the turn's job starts and drops it as the job ends.
type grant struct {
	session string
	can     map[string]bool // address/fn
}

// watcher is a client subscribed to topics, every topic if topics is nil.
// Its events wait in ch for the client's connection; when ch is full they
// are dropped, so a slow client never holds up a publisher.
type watcher struct {
	topics map[string]bool
	ch     chan rpc.Event
}

// route owns the state until the daemon stops; others reach it through do.
// Its history writer stores what is left once it stops, before whiff.db
// closes.
func (d *daemon) route() {
	kick, final := make(chan struct{}, 1), make(chan batch, 1)
	d.wg.Go(func() { d.writeHistory(kick, final) })
	st := &state{db: d.db, log: d.log, kick: kick, addrs: map[string]*service{}, jobs: map[int64]*job{}, waiters: map[int64][]chan puff.Value{},
		subs: map[*watcher]bool{}, tracers: map[*tracer]bool{}, modes: map[string]*mode{}, grants: map[string]grant{}}
	defer func() { final <- st.takeUnstored() }()
	st.load()
	for {
		select {
		case op := <-d.ops:
			op(st)
		case <-d.ctx.Done():
			return
		}
	}
}

// do runs op on the route goroutine and waits for it. It reports false if
// the daemon is stopping and op didn't run.
func (d *daemon) do(op func(st *state)) bool {
	done := make(chan struct{})
	select {
	case d.ops <- func(st *state) { op(st); close(done) }:
	case <-d.ctx.Done():
		return false
	}
	<-done
	return true
}

// msgID is a new id for a message or an eval in history, which the
// messages it leads to have as their cause.
func (st *state) msgID() int64 {
	st.lastMsg++
	return st.lastMsg
}

// deliver puts m in addr's mailbox, or its inbox for an ask function, or
// returns the (err e) for its sender and records it in history.
func (d *daemon) deliver(addr string, m message) puff.Value {
	var res puff.Value
	if !d.do(func(st *state) { res = d.put(st, addr, m) }) {
		return puff.Errorf("no-address", "whiff is stopping")
	}
	return res
}

// put is deliver on the route goroutine. A message to an ask function,
// or one the review gate holds, goes to an inbox and is answered (err
// {:kind :pending :id n}) at once.
func (d *daemon) put(st *state, addr string, m message) puff.Value {
	if m.id == 0 {
		m.id = st.msgID()
	}
	var res puff.Value
	// An agent session's mode may let its message through, as accepted.
	if !m.user && !m.reviewed && st.lets(m.from, addr) {
		m.reviewed = true
		if s := st.addrs[addr]; s != nil && m.topic == "" && (&marker{base: d.base, services: st.addrs}).msgMark(s, m) == puff.MarkWrite {
			m.gate = st.modeOf(m.from).name()
		}
	}
	gate := m.gate
	switch s := st.addrs[addr]; {
	case s == nil:
		res = puff.Errorf("no-address", "no address :%s", addr)
	case d.gated(st, s, m):
		res = d.hold(st, addr, m)
		gate = heldGate(res)
	case m.topic == "" && s.fns[m.fn].ask:
		res = d.ask(st, s, m)
	default:
		select {
		case s.mailbox <- m:
			return nil
		default:
			res = puff.Errorf("full", ":%s's mailbox is full", addr)
		}
	}
	st.record(entry{at: time.Now(), kind: m.kind(), id: m.id, cause: m.cause, user: m.user, from: m.from, to: m.to(addr), args: m.args, reply: puff.Print(res), gate: gate})
	return res
}

// publish puts m, for a topic, in the mailbox of every service subscribed to
// it, and sends it to every client that is. A full mailbox is recorded in
// history; the publisher doesn't wait.
func (d *daemon) publish(m message) {
	d.do(func(st *state) { st.publish(m) })
}

// publish is the daemon's publish on the route goroutine.
func (st *state) publish(m message) {
	var e *rpc.Event
	for w := range st.subs {
		if w.topics != nil && !w.topics[m.topic] {
			continue
		}
		if e == nil {
			v, _ := puff.ReadValue(m.args)
			e = &rpc.Event{At: time.Now(), Topic: m.topic, Value: puff.Print(v.(puff.Vector).At(0)), From: m.from, Origin: string(origin(m.user))}
		}
		select {
		case w.ch <- *e:
		default:
		}
	}
	for addr, s := range st.addrs {
		if _, ok := s.subs[m.topic]; !ok {
			continue
		}
		m.id = st.msgID()
		select {
		case s.mailbox <- m:
		default:
			res := puff.Errorf("full", ":%s's mailbox is full", addr)
			st.record(entry{at: time.Now(), kind: m.kind(), id: m.id, cause: m.cause, user: m.user, from: m.from, to: m.to(addr), args: m.args, reply: puff.Print(res), gate: m.gate})
		}
	}
}

// builtins are whiff's functions on top of puff's base table, acting for
// cur. What a service does with messages is bare, as puff's core is: call,
// send, publish, reply...; what inspects or manages whiff is in a module:
// address/, service/, session/. call, send, send-after and publish are
// :write: in a handler, one with
// a literal target or topic counts as its target's mark instead (see
// marker), and under a :read mark they fault only on a :write target.
func (d *daemon) builtins(cur *caller) []*puff.Builtin {
	return append([]*puff.Builtin{
		{Name: "address/describe", Args: "address", Mark: puff.MarkRead,
			Doc: "(ok fns): address's functions, each {:fn :params :doc :mark :form}, and :yours #t on one only your connections may use.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, ok := args[0].(puff.Keyword)
				if !ok {
					return nil, fmt.Errorf("want an address like :gh, got %s", puff.Print(args[0]))
				}
				return d.marker().describe(string(addr)), nil
			}},
		{Name: "service/reset", Args: "service", Mark: puff.MarkWrite,
			Doc: "Deletes service's stored state and reloads it from its file, so it starts from its :state again: (ok :done), or (err e).",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, ok := args[0].(puff.Keyword)
				if !ok {
					return nil, fmt.Errorf("want a service like :gh, got %s", puff.Print(args[0]))
				}
				if string(addr) == cur.addr {
					// Its reload would wait for this handler to end.
					return puff.Errorf("bad-call", "a service can't reset itself"), nil
				}
				r := reset{name: string(addr), done: make(chan puff.Value, 1)}
				select {
				case d.resets <- r:
				case <-c.Ctx.Done():
					return nil, c.Ctx.Err()
				}
				return <-r.done, nil
			}},
		{Name: "address/list", Mark: puff.MarkRead,
			Doc: "(ok addrs): every address, sorted, each {:address :client :state}: :client for a client's, :state for a service with :state.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var addrs []puff.Value
				d.do(func(st *state) {
					for _, name := range slices.Sorted(maps.Keys(st.addrs)) {
						s := st.addrs[name]
						m, _ := puff.NewMap(
							puff.Entry{Key: puff.Keyword("address"), Val: puff.Keyword(name)},
							puff.Entry{Key: puff.Keyword("client"), Val: puff.Bool(s.client)},
							puff.Entry{Key: puff.Keyword("state"), Val: puff.Bool(s.stateful)},
						)
						addrs = append(addrs, m)
					}
				})
				return puff.Ok(puff.NewVector(addrs...)), nil
			}},
		{Name: "service/state", Args: "service", Mark: puff.MarkRead,
			Doc: "(ok state): service's state as it last committed it; (err {:kind :stateless}) for a service without :state, (err {:kind :no-address}) if there is none.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, ok := args[0].(puff.Keyword)
				if !ok {
					return nil, fmt.Errorf("want a service like :gh, got %s", puff.Print(args[0]))
				}
				return d.stateOf(string(addr))
			}},
		{Name: "inbox", Args: "[fn]", Mark: puff.MarkRead,
			Doc: "This service's messages waiting for a reply, oldest first, each {:id :fn :args :from :cwd :at}; only fn's, a keyword, if given.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service has an inbox")
				}
				var fn puff.Keyword
				if len(args) > 0 {
					var ok bool
					if fn, ok = args[0].(puff.Keyword); !ok {
						return nil, fmt.Errorf("want a function name like :suggest, got %s", puff.Print(args[0]))
					}
				}
				var items []puff.Value
				d.do(func(st *state) {
					for _, a := range st.inbox(cur.addr) {
						if fn == "" || a.fn == string(fn) {
							items = append(items, a.value())
						}
					}
				})
				return puff.NewVector(items...), nil
			}},
		// reply is :read: an answer from an agent's chain is :agent's and
		// not reviewed, so what its sender does with it is gated there.
		{Name: "reply", Args: "id result", Mark: puff.MarkRead,
			Doc: "Answer the message id in this service's inbox with result: its sender gets [:whiff/done id result]. (err {:kind :no-id}) if it isn't there.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service has an inbox to reply from: a client answers with whiff reply")
				}
				return d.reply(cur.addr, cur.user, args[0], args[1])
			}},
		{Name: "self", Mark: puff.MarkRead,
			Doc: "This service's address, as a keyword: :assistant, or :try/3/assistant in whiff try.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service, or a client that holds an address, has one")
				}
				return puff.Keyword(cur.addr), nil
			}},
		{Name: "session/mode", Mark: puff.MarkRead,
			Doc: "(ok m): this agent session's mode, {:mode :gated}, {:mode :free} or {:mode :trusted :services [:todo ...]}, with :until, a time, if it runs out. (err {:kind :no-session}) for anyone else.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var r puff.Value
				d.do(func(st *state) { r = st.modeValue(cur.addr) })
				return r, nil
			}},
		{Name: "hand-on", Args: "r", Mark: puff.MarkRead,
			Doc: "r; if r is the pending of a job or ask this service started, its answer goes to the caller of the message being handled instead. (err {:kind :no-id}) for another pending.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.handOn == nil {
					return nil, errors.New("only a service's handler can hand on")
				}
				id, ok := pendingID(args[0])
				if !ok {
					return args[0], nil
				}
				if err := cur.handOn(id); err != nil {
					return puff.Errorf("no-id", "%v", err), nil
				}
				return args[0], nil
			}},
		// submitted is the one place a pending becomes a successful
		// submission. A held one counts: it keeps its id from held to
		// ended, so the same whiff/done answers it, with :rejected if you
		// reject it.
		{Name: "submitted", Args: "r", Mark: puff.MarkRead,
			Doc: "(ok id) if r is a pending, of a job, an ask or a message the review gate holds: it was accepted, and [:whiff/done id result] answers it; else r, so a failure to submit stays an err. hand-on wants the pending itself.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if id, ok := pendingID(args[0]); ok {
					return puff.Ok(puff.Int(id)), nil
				}
				return args[0], nil
			}},
		{Name: "publish", Args: "topic v", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send v to every service subscribed to topic, a keyword: :done at once; pending if the review gate holds it.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				topic, ok := args[0].(puff.Keyword)
				if !ok || topic == "" {
					return nil, fmt.Errorf("want a topic like :git/changed, got %s", puff.Print(args[0]))
				}
				if what, ok := puff.ReadOnly(c.Ctx); ok {
					mk := d.marker()
					mk.seen = map[string]bool{}
					if mk.topic(string(topic)) == puff.MarkWrite {
						return nil, fmt.Errorf(":%s has :write subscribers, and %s is :read", topic, what)
					}
				}
				msg, err := encode(puff.NewVector(args[1]))
				if err != nil {
					return nil, err
				}
				m := message{cause: cur.id, from: cur.addr, topic: string(topic), args: msg, dir: c.Env.Dir, user: cur.user, reviewed: cur.reviewed, session: cur.session}
				return d.publishGated(m), nil
			}},
		{Name: "call", Args: "target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := cur.message(c.Env.Dir, args)
				if err == nil {
					err = d.readOnly(c.Ctx, addr, m)
				}
				if err != nil {
					return nil, err
				}
				return d.request(c.Ctx, cur, addr, m)
			}},
		{Name: "send", Args: "target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target, :address/fn, a message without waiting: :done once it is in the mailbox.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := cur.message(c.Env.Dir, args)
				if err == nil {
					err = d.readOnly(c.Ctx, addr, m)
				}
				if err != nil {
					return nil, err
				}
				if e := d.deliver(addr, m); e != nil {
					return e, nil
				}
				return puff.Done, nil
			}},
		{Name: "send-after", Args: "d target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target a message once d has passed; :done at once.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				after, ok := args[0].(puff.Duration)
				if !ok || after < 0 {
					return nil, fmt.Errorf("want a duration of 0s or more, got %s", puff.Print(args[0]))
				}
				m, addr, err := cur.message(c.Env.Dir, args[1:])
				if err == nil {
					err = d.readOnly(c.Ctx, addr, m)
				}
				if err != nil {
					return nil, err
				}
				// A timer's message begins a chain of its own in history, or
				// a service that schedules itself would be one endless chain;
				// it keeps its origin. A service's timers to itself go with
				// the version that set them, so a reload, whose :start sets
				// its own, doesn't double a loop. It is set only once it is
				// stored.
				m.cause = 0
				t := &timer{due: time.Now().Add(time.Duration(after)), addr: addr, m: m}
				if err := d.keepTimer(t); err != nil {
					return puff.Errorf("store", "the timer for :%s wasn't stored: %v", m.to(addr), err), nil
				}
				var stop, placed <-chan struct{}
				if t.own() {
					stop, placed = cur.stop, cur.placed
				}
				d.arm(t, stop, placed)
				return puff.Done, nil
			}},
	}, viewBuiltins()...)
}

// Functions are the names of whiff's functions, sorted: what puff tells
// you are only in whiff.
func Functions() []string {
	var names []string
	for _, b := range (&daemon{}).builtins(&caller{}) {
		names = append(names, b.Name)
	}
	slices.Sort(names)
	return names
}

// request sends m to addr for cur and waits for its reply: the target's
// Result, or an (err e) when there is none. A call to an address already
// waiting in the chain would wait for itself, so it fails at once. The
// error is for ctx ending.
func (d *daemon) request(ctx context.Context, cur *caller, addr string, m message) (puff.Value, error) {
	target := ":" + addr + "/" + m.fn
	if slices.Contains(cur.chain, addr) {
		var path strings.Builder
		for _, a := range cur.chain {
			fmt.Fprintf(&path, ":%s -> ", a)
		}
		path.WriteString(":" + addr)
		return puff.Errorf("cycle", "%s would wait for itself: %s", target, path.String()), nil
	}
	wait, cancel := context.WithTimeout(ctx, d.callTimeout)
	defer cancel()
	m.ctx = wait
	m.chain = cur.chain
	m.reply = make(chan string, 1)
	if e := d.deliver(addr, m); e != nil {
		return e, nil
	}
	select {
	case r := <-m.reply:
		return puff.ReadValue(r)
	case <-wait.Done():
		if ctx.Err() != nil {
			return nil, errors.New("cancelled")
		}
		return puff.Errorf("timeout", "%s didn't reply within %v", target, d.callTimeout), nil
	}
}

// message builds the message args, a target and its arguments, ask for,
// sent from dir in cur's chain, and returns it with the target's address.
// A send-after builds it when it is scheduled, so it keeps that origin.
func (cur *caller) message(dir string, args []puff.Value) (message, string, error) {
	addr, fn, err := cur.target(args[0])
	if err != nil {
		return message{}, "", err
	}
	msg, err := encode(puff.NewVector(args[1:]...))
	if err != nil {
		return message{}, "", err
	}
	return message{cause: cur.id, from: cur.addr, fn: fn, args: msg, dir: dir, user: cur.user, reviewed: cur.reviewed, session: cur.session}, addr, nil
}

// readOnly is an error if ctx runs under a :read mark and m, a message to
// addr, is :write, unless its sender's mode lets it through, as it does a
// trusted session's eval's. A target that doesn't exist is left for m to
// fail.
func (d *daemon) readOnly(ctx context.Context, addr string, m message) error {
	what, ok := puff.ReadOnly(ctx)
	if !ok {
		return nil
	}
	mk := d.marker()
	s, ok := mk.services[addr]
	if !ok {
		return nil
	}
	if _, ok := s.fns[m.fn]; !ok || mk.msgMark(s, m) == puff.MarkRead {
		return nil
	}
	var lets bool
	d.do(func(st *state) { lets = !m.user && st.lets(m.from, addr) })
	if lets {
		return nil
	}
	return fmt.Errorf(":%s/%s is :write, and %s is :read", addr, m.fn, what)
}

// target splits :address/fn at its last slash: :gh/prs, :agent/1/inbox.
// A bare :fn is a function of cur's own address.
func (cur *caller) target(v puff.Value) (addr, fn string, err error) {
	k, ok := v.(puff.Keyword)
	if !ok || k == "" {
		return "", "", fmt.Errorf("want a target :address/fn, got %s", puff.Print(v))
	}
	i := strings.LastIndexByte(string(k), '/')
	switch {
	case i < 0 && cur.addr == "":
		return "", "", fmt.Errorf("%s has no address: only a service can leave it out, want :address/fn", puff.Print(v))
	case i < 0:
		return cur.addr, string(k), nil
	case i == 0 || i == len(k)-1:
		return "", "", fmt.Errorf("want a target :address/fn, got %s", puff.Print(v))
	}
	return string(k[:i]), string(k[i+1:]), nil
}

// encode prints v as a message. Functions don't print as data, so a value
// holding one can't be sent.
func encode(v puff.Value) (string, error) {
	s := puff.Print(v)
	if _, err := puff.ReadValue(s); err != nil {
		return "", fmt.Errorf("can't send %s: only data travels", s)
	}
	return s, nil
}
