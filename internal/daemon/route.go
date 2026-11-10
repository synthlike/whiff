package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// there is where a message to a peer runs there, whiff call -C's,
	// instead of the peer's dir; read is what runs under a :read mark
	// where it was sent, so the peer refuses a :write target.
	there, read string
	// chain is the addresses waiting on calls that led to this one, so a
	// call back into one of them fails at once instead of timing out.
	chain []string
	lineage
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

// lineage is how the chain a message belongs to began, and whether the
// review gate let it through: what the gate decides on. Messages carry
// it, callers act in it, and tickets answer into it, so it is copied
// whole; a copy that drops reviewed says so with unreviewed.
type lineage struct {
	// user is whether the chain began with the user; one without a clear
	// origin is the agent's.
	user bool
	// reviewed is whether you accepted a message of the chain at the
	// review gate, or gave the answer it goes on with, which lets it
	// through.
	reviewed bool
	// session is the agent session, agent/<n>, whose message began the
	// chain; "" for a chain that began elsewhere.
	session string
	// task is the task whose work the chain is: the one whose whiff/asked
	// or whiff/cancelled began it, or the job whose session sent it, as an
	// agent's turn; 0 for none. A task begun in the chain is its child, and
	// its ticket keeps task as its parent, so its whiff/done goes on in the
	// parent's work.
	task int64
	// project is the project the chain's work is in, as projectOf names
	// it: set by the first job run in a repository, or the session of one,
	// and carried on from there; "" until then.
	project string
}

// unreviewed is l without your acceptance: for a message the review gate
// holds, which waits for it.
func (l lineage) unreviewed() lineage {
	l.reviewed = false
	return l
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

// notice says whether m is whiff telling a service of something already
// begun: a whiff/done, whiff/asked or whiff/cancelled.
func (m message) notice() bool {
	switch m.fn {
	case "whiff/done", "whiff/asked", "whiff/cancelled":
		return m.topic == ""
	}
	return false
}

// entry is m's entry in history, at at, delivered to, to, with its
// reply and how the review gate let it through, if it did.
func (m message) entry(at time.Time, to, reply, gate string) entry {
	return entry{at: at, kind: m.kind(), id: m.id, cause: m.cause, user: m.user, from: m.from, to: to, args: m.args, reply: reply, gate: gate, project: m.project}
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
	addr    string
	id      int64    // the message being handled, or the eval running, that the messages sent are caused by; 0 for none
	chain   []string // addresses waiting in the current chain, addr last
	lineage          // the current chain's
	// stop is closed when the service version acting stops; nil for a
	// client. placed is closed once that version has its address.
	stop, placed <-chan struct{}
	// handOn gives the sender of the message being handled the answer to
	// a job or ask; nil outside a handler.
	handOn func(id int64) error
}

// state is what the route goroutine owns.
type state struct {
	db         *bbolt.DB // whiff.db: see store.go
	log        io.Writer
	addrs      map[string]*service
	history    []entry         // the last historySize entries, oldest first
	unstored   []entry         // history the writer hasn't taken: see record
	dropped    int             // entries left out of unstored since it was last taken
	kick       chan<- struct{} // wakes the history writer
	asked      []*asked        // every service's inbox, oldest first
	timers     []*timer        // what load read, until resume takes it up
	lost       []*job          // what load read, until resume takes it up
	owed       []answered      // results for services that aren't loaded, until they are
	cut        []ticket        // what load read, until resume takes it up: accepted as whiff went
	jobs       map[int64]*job
	nextID     int64                       // the last id given to a message answered later, or a job
	idsTo      int64                       // the last id stored as reserved: newID gives up to it without the disk
	lastMsg    int64                       // the last id given to a message or an eval in history: see msgID
	waiters    map[int64][]chan puff.Value // whiff waits, by the id they wait for
	subs       map[*watcher]bool           // clients' subscriptions: whiff sub
	tracers    map[*tracer]bool            // clients following history: whiff history -f
	modes      map[string]*mode            // agent sessions that aren't gated, by address
	grants     map[string]grant            // agent turns' session tokens: see grant
	health     map[string]*health          // by service file's name: see whiff services
	peers      map[string]*peer            // other machines' whiffs, by name: see peers.go
	keyed      map[string]chan struct{}    // submission keys whose message is running, closed as it ends: see once
	checking   map[int64]bool              // tasks following a peer's whose result is being asked for: see checkFar
	checkAgain map[int64]bool              // of those, the ones to ask about again once that is done
}

// grant is what a job's session token lets its whiff mcp claim: the
// session, :agent/<the agent's name> for an agent's turn, or "" for a new
// :agent/<n>, and the functions it may call.
// whiff makes one as the turn's job starts and drops it as the job ends.
type grant struct {
	session string
	can     map[string]bool // address/fn
	// claimed are the new sessions, agent/<n>, claimed with it, for a job
	// of a service that isn't an agent: they end with the job.
	claimed []string
	// job is the job given it, the parent of what its sessions begin.
	job int64
}

// watcher is a client subscribed to topics, every topic if topics is nil.
// Its events wait in ch for the client's connection; when ch is full they
// are dropped, so a slow client never holds up a publisher.
type watcher struct {
	topics map[string]bool
	ch     chan rpc.Event
	agent  bool // an agent's connection: it doesn't review
}

// route owns the state until the daemon stops; others reach it through do.
// Its history writer stores what is left once it stops, before whiff.db
// closes.
func (d *daemon) route() {
	kick, final := make(chan struct{}, 1), make(chan batch, 1)
	d.wg.Go(func() { d.writeHistory(kick, final) })
	st := &state{db: d.db, log: d.log, kick: kick, addrs: map[string]*service{}, jobs: map[int64]*job{}, waiters: map[int64][]chan puff.Value{}, peers: map[string]*peer{}, keyed: map[string]chan struct{}{}, checking: map[int64]bool{}, checkAgain: map[int64]bool{},
		subs: map[*watcher]bool{}, tracers: map[*tracer]bool{}, modes: map[string]*mode{}, grants: map[string]grant{}, health: map[string]*health{}}
	defer func() {
		// Where each peer's history is up to: followFar can't store it
		// once whiff stops.
		for _, p := range st.peers {
			if err := storeCursor(st.db, p); err != nil {
				st.note(fmt.Sprintf("peer %s's place in its history isn't stored, so some of it may come again: %v", p.name, err))
			}
		}
		final <- st.takeUnstored()
	}()
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
		if s := st.addrs[addr]; s != nil && m.topic == "" && d.markerOn(st).msgMark(s, m) == puff.MarkWrite {
			m.gate = st.modeOf(m.from).name()
		}
	}
	if _, _, ok := farAddr(addr); ok {
		return d.forward(st, addr, m)
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
	st.record(m.entry(time.Now(), m.to(addr), puff.Print(res), gate))
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
	v, _ := puff.ReadValue(m.args)
	vec, ok := v.(puff.Vector)
	if !ok || vec.Len() != 1 {
		st.note(fmt.Sprintf("an event on :%s isn't one value, so it isn't published: %s", m.topic, m.args))
		return
	}
	var e *rpc.Event
	for w := range st.subs {
		if w.topics != nil && !w.topics[m.topic] {
			continue
		}
		if e == nil {
			e = &rpc.Event{At: time.Now(), Topic: m.topic, Value: puff.Print(vec.At(0)), From: m.from, Origin: string(origin(m.user))}
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
			st.record(m.entry(time.Now(), m.to(addr), puff.Print(res), m.gate))
		}
	}
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
	// A peer's function has the call's time once its peer is connected.
	timeout := d.callTimeout
	if _, _, far := farAddr(addr); far {
		timeout += farConnectWait
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
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
		return puff.Errorf("timeout", "%s didn't reply within %v", target, timeout), nil
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
	return message{cause: cur.id, from: cur.addr, fn: fn, args: msg, dir: dir, lineage: cur.lineage}, addr, nil
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
