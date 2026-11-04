package daemon

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// The review gate: an agent's message to a :write function waits in
// :review's inbox until you accept it, which delivers it as it was sent, or
// reject it. Its sender gets (err {:kind :pending :id n}) at once and the
// answer later, as [:whiff/done n result], as for an ask. Your own messages
// go straight through, and so does every message in the chain of one you
// accepted.

// gated says whether the review gate holds m, for s: an agent's message to
// a :write function that you haven't accepted. Not an ask, which waits for
// a person anyway, nor a whiff/done, the answer to something s started,
// nor a function only you may use, which refuses it anyway.
func (d *daemon) gated(st *state, s *service, m message) bool {
	if m.user || m.reviewed || m.topic != "" || m.fn == "whiff/done" || m.fn == "whiff/asked" || m.fn == "whiff/cancelled" {
		return false
	}
	h, ok := s.fns[m.fn]
	if !ok || h.ask || h.yours {
		return false
	}
	// An agent's turn has no effects of its own: no tools but its :can,
	// whose writes its session's mode gates, and dollars its :limits cap.
	if from := st.addrs[m.from]; s.name == "jobs" && m.fn == "run" && from != nil && from.agent {
		return false
	}
	mk := &marker{base: d.base, services: st.addrs}
	return mk.msgMark(s, m) == puff.MarkWrite
}

// forSession says whether m, which s handled, is a write a service made
// further down a chain an agent session began and the review gate let
// through, as trusted, free or accepted. History notes it as for the
// session, so whiff session lists what services did for it too.
func (d *daemon) forSession(st *state, s *service, m message) bool {
	if m.user || !m.reviewed || m.session == "" || m.from == m.session {
		return false
	}
	mk := &marker{base: d.base, services: st.addrs, seen: map[string]bool{}}
	if m.topic != "" {
		return mk.sub(s, s.subs[m.topic]) == puff.MarkWrite
	}
	return mk.msgMark(s, m) == puff.MarkWrite
}

// publishGated publishes m, as publish and whiff pub do: :done, or the
// pending of the review gate, which holds an agent's publish to a topic
// with :write subscribers as it holds a message. whiff's own events, such
// as :jobs/done, aren't gated: like a whiff/done, they tell of something
// already started.
func (d *daemon) publishGated(m message) puff.Value {
	r := puff.Done
	d.do(func(st *state) {
		if !m.user && !m.reviewed {
			mk := &marker{base: d.base, services: st.addrs, seen: map[string]bool{}}
			write := mk.topic(m.topic) == puff.MarkWrite
			switch {
			case st.lets(m.from, ""):
				m.reviewed = true
				if write {
					m.gate = "free"
				}
			case write:
				m.id = st.msgID()
				r = d.hold(st, "", m)
				st.record(entry{at: time.Now(), kind: "pub", id: m.id, cause: m.cause, from: m.from, to: "review on :" + m.topic, args: m.args, reply: puff.Print(r), gate: heldGate(r)})
				return
			}
		}
		st.publish(m)
	})
	return r
}

// hold puts m, for addr, or m's topic, in :review's inbox: see queue.
func (d *daemon) hold(st *state, addr string, m message) puff.Value {
	v, err := puff.ReadValue(m.args)
	if err != nil {
		return puff.Errorf("bad-call", "%v", err)
	}
	held := m
	held.ctx, held.reply, held.chain = nil, nil, nil
	a := &asked{ticket: ticket{msg: m.id, from: m.from, dir: m.dir, lineage: m.unreviewed()}, fn: m.fn, args: v.(puff.Vector), held: &held, to: addr}
	if m.topic != "" {
		a.fn, a.to = "publish", m.topic
	}
	return st.queue(a, a.what())
}

// holdEval puts an agent's code, req.Src, evaluated for from, in :review's
// inbox as {:fn :eval :args [src]}: see queue. Accepting runs it.
func (d *daemon) holdEval(st *state, from string, req rpc.Request) puff.Value {
	a := &asked{ticket: ticket{msg: st.msgID(), from: from, dir: req.Dir, lineage: lineage{session: from}}, fn: "eval", args: puff.NewVector(puff.String(req.Src)), src: req.Src}
	r := st.queue(a, "the agent's eval")
	st.record(entry{at: time.Now(), kind: "eval", id: a.msg, from: from, args: puff.Print(a.args), reply: puff.Print(r), gate: heldGate(r)})
	return r
}

// queue puts a, which what names, in :review's inbox and publishes it on
// :review/pending, and returns the pending for its sender; or says why it
// can't wait there: nobody follows :review/pending to review it, the inbox
// is full, or it can't be stored.
func (st *state) queue(a *asked, what string) puff.Value {
	if !st.reviewing() {
		return puff.Errorf("no-reviewer", "%s is :write, and nobody reviews agents' messages: a reviewer follows :review/pending (whiff sub review/pending)", what)
	}
	if len(st.inbox("review")) >= inboxSize {
		return puff.Errorf("full", ":review's inbox is full")
	}
	if e := st.tooMany(a.from); e != nil {
		return e
	}
	a.id, a.addr, a.at = st.newID(), "review", time.Now()
	if err := st.keep(a); err != nil {
		return puff.Errorf("store", "%s wasn't stored: %v", what, err)
	}
	st.asked = append(st.asked, a)
	st.publish(message{cause: a.msg, from: "review", topic: "review/pending", args: puff.Print(puff.NewVector(a.value())), lineage: lineage{user: a.user}})
	return pending(a.id, "%s waits for your review", what)
}

// reviewing says whether a client follows :review/pending, on its own or
// with every topic.
func (st *state) reviewing() bool {
	for w := range st.subs {
		if w.topics == nil || w.topics["review/pending"] {
			return true
		}
	}
	return false
}

// accept delivers the message id that :review holds, as it was sent and
// let through the gate with all it leads to, or runs the code it holds,
// and returns its result, which its sender gets too. The error is for ctx
// ending. chain is the addresses waiting on the accept: :review, for its
// own accept, so what the message leads to fails at once if it calls
// :review back, rather than waiting for itself. What it delivers or runs
// is new in history, caused by what was held.
func (d *daemon) accept(ctx context.Context, chain []string, id int64) (puff.Value, error) {
	var a *asked
	var next int64
	var stored error
	d.do(func(st *state) { a, stored = st.acceptHeld(id); next = st.msgID() })
	switch {
	case stored != nil:
		return puff.Errorf("store", "message %d stays held: it couldn't be stored as accepted: %v", id, stored), nil
	case a == nil:
		return puff.Errorf("no-id", "no message %d waits for review", id), nil
	}
	if a.held == nil {
		r := d.runHeld(ctx, chain, a, next)
		d.do(func(st *state) {
			st.record(entry{at: a.at, kind: "eval", id: next, cause: a.msg, from: a.from, args: puff.Print(a.args), reply: puff.Print(r), gate: fmt.Sprintf("accepted %d", a.id)})
		})
		d.reviewed(a, r)
		return r, ctx.Err()
	}
	m := *a.held
	m.id, m.cause, m.held = next, a.msg, a.id
	m.gate = fmt.Sprintf("accepted %d", a.id)
	if m.topic != "" {
		m.reviewed = true
		d.publish(m)
		d.reviewed(a, puff.Done)
		return puff.Done, nil
	}
	m.reviewed = true
	r, err := d.request(ctx, &caller{chain: chain}, a.to, m)
	if err != nil {
		r = failed("cancelled", "unknown", "the accept was cancelled before :%s replied", m.to(a.to))
	}
	d.reviewed(a, r)
	return r, err
}

// runHeld runs the code a holds, as its sender's eval would have and
// passing the gate, within the call timeout. Its Result is the code's
// value, (ok v) unless it is a Result, or a fault as (err {:kind :fault}).
// What it prints is dropped. chain is accept's; id is the eval's in
// history.
func (d *daemon) runHeld(ctx context.Context, chain []string, a *asked, id int64) puff.Value {
	env := d.newEnv(io.Discard, &caller{addr: a.from, id: id, chain: chain, lineage: lineage{reviewed: true, session: a.session}})
	env.Dir = a.dir
	ctx, cancel := context.WithTimeout(ctx, d.callTimeout)
	defer cancel()
	v, err := env.Load(ctx, "eval", a.src)
	if err != nil {
		return puff.Errorf("fault", "%v", err)
	}
	if t, ok := v.(puff.Tag); ok && t.Len() == 1 && (t.Name() == "ok" || t.Name() == "err") {
		return v
	}
	if v == nil {
		return puff.Done
	}
	return puff.Ok(v)
}

// reject answers the message id that :review holds (err {:kind :rejected
// :reason reason}) without delivering it; without :reason if reason is "".
func (d *daemon) reject(id int64, reason string) puff.Value {
	var a *asked
	d.do(func(st *state) { a = st.take("review", id) })
	if a == nil {
		return puff.Errorf("no-id", "no message %d waits for review", id)
	}
	msg := "you rejected " + a.what()
	es := []puff.Entry{{Key: puff.Keyword("kind"), Val: puff.Keyword("rejected")}}
	if reason != "" {
		msg += ": " + reason
		es = append(es, puff.Entry{Key: puff.Keyword("reason"), Val: puff.String(reason)})
	}
	e, _ := puff.NewMap(append(es,
		puff.Entry{Key: puff.Keyword("message"), Val: puff.String(msg)},
		puff.Entry{Key: puff.Keyword("outcome"), Val: puff.Keyword("not-started")})...)
	d.reviewed(a, puff.NewTag("err", e))
	return puff.Done
}

// reviewed answers a's sender with result, and publishes {:id :result} on
// :review/done.
func (d *daemon) reviewed(a *asked, result puff.Value) {
	d.do(func(st *state) { d.settleHeld(st, a, result) })
}

// settleHeld is reviewed on the route goroutine. A job that a started kept
// a's id, so it answers it when it ends: result is its pending, or, if the
// accept was cancelled after it started, not what happened.
func (d *daemon) settleHeld(st *state, a *asked, result puff.Value) {
	if id, ok := pendingID(result); st.jobs[a.id] == nil && (!ok || id != a.id) {
		d.answer(st, a.ticket, a.to, result)
	}
	done, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("id"), Val: puff.Int(a.id)},
		puff.Entry{Key: puff.Keyword("result"), Val: result},
	)
	st.publish(message{cause: a.msg, from: "review", topic: "review/done", args: puff.Print(puff.NewVector(done)), lineage: lineage{user: a.user}})
}

// reviewService is :review, whose inbox holds what the review gate holds:
// its functions show it as a view and accept or reject a message, as whiff
// pending, accept and reject do, so any client that sends messages can
// review. They are yours: only your connections may use them, as only
// yours may run whiff accept.
func (d *daemon) reviewService() *service {
	return &service{name: "review", d: d, cur: caller{addr: "review"},
		fns: map[string]handler{
			"pending": {mark: puff.MarkRead, marked: true, yours: true, native: d.pendingView,
				doc: "(ok view): what waits for your review, as items to accept or reject, oldest first."},
			"accept": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, yours: true, native: d.acceptFn, apart: true,
				doc: "Deliver the held message id as it was sent, or run the code it holds; its result, which its sender gets too."},
			"reject": {params: []string{"id", "reason"}, optional: 1, mark: puff.MarkWrite, marked: true, yours: true, native: d.rejectFn,
				doc: "Answer the held message id (err {:kind :rejected :reason reason}) without delivering it; reason, a string, tells its sender why."},
			"trust": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, yours: true, native: d.trustFn, apart: true,
				doc: "Accept the held message id, an agent session's to a service, and trust the session for that service from now on, as whiff trust does."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

// pendingView is :review's inbox as (items [...]), each item a held message
// with Accept and Reject, fetched again when the queue changes.
func (d *daemon) pendingView(message, []puff.Value) (puff.Value, error) {
	var items []puff.Value
	d.do(func(st *state) {
		for _, a := range st.inbox("review") {
			items = append(items, a.reviewItem(st.addrs[a.from] != nil))
		}
	})
	opts, _ := puff.NewMap(puff.Entry{Key: puff.Keyword("changed"),
		Val: puff.NewVector(puff.Keyword("review/pending"), puff.Keyword("review/done"))})
	return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
}

// trustable says whether a is an agent session's message to a service,
// which :review/trust can let through from now on.
func (a *asked) trustable() bool {
	return a.held != nil && a.held.topic == "" && strings.HasPrefix(a.from, "agent/")
}

// reviewItem is a, held, as an item of :review/pending: what it would do,
// who sent it and where, and its actions: Trust too, if it is trustable and
// its session, live, is still here to trust.
func (a *asked) reviewItem(live bool) puff.Value {
	label := a.what()
	switch {
	case a.held == nil:
		label = "eval " + a.src
	case a.args.Len() > 0:
		label += " " + strings.TrimSuffix(strings.TrimPrefix(puff.Print(a.args), "["), "]")
	}
	from := "client"
	if a.from != "" {
		from = ":" + a.from
	}
	action := func(label, fn string) puff.Value {
		m, _ := puff.NewMap(
			puff.Entry{Key: puff.Keyword("label"), Val: puff.String(label)},
			puff.Entry{Key: puff.Keyword("action"), Val: puff.NewVector(puff.Keyword("review/"+fn), puff.Int(a.id))},
		)
		return m
	}
	reject, _ := action("Reject", "reject").(puff.Map).With(puff.Keyword("ask"), puff.String("Why? Its sender reads this."))
	actions := []puff.Value{action("Accept", "accept"), reject}
	if a.trustable() && live {
		actions = append(actions, action(fmt.Sprintf("Trust :%s for :%s", a.from, a.to), "trust"))
	}
	m, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("label"), Val: puff.String(label)},
		puff.Entry{Key: puff.Keyword("detail"), Val: puff.String(fmt.Sprintf("%d from %s in %s", a.id, from, a.dir))},
		puff.Entry{Key: puff.Keyword("actions"), Val: puff.NewVector(actions...)},
	)
	return m
}

// trustFn trusts the agent session that sent the held message id for its
// service, and accepts it.
func (d *daemon) trustFn(m message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a message id, got %s", puff.Print(args[0]))
	}
	var r puff.Value
	d.do(func(st *state) {
		i := slices.IndexFunc(st.inbox("review"), func(a *asked) bool { return a.id == int64(id) })
		switch {
		case i < 0:
			r = puff.Errorf("no-id", "no message %d waits for review", id)
		case !st.inbox("review")[i].trustable():
			r = puff.Errorf("bad-call", "message %d isn't an agent session's message to a service", id)
		default:
			a := st.inbox("review")[i]
			r = st.trust(a.from, a.to)
		}
	})
	if !puff.Equal(r, puff.Done) {
		return r, nil
	}
	r, _ = d.accept(m.ctx, []string{"review"}, int64(id))
	return r, nil
}

func (d *daemon) acceptFn(m message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a message id, got %s", puff.Print(args[0]))
	}
	r, _ := d.accept(m.ctx, []string{"review"}, int64(id))
	return r, nil
}

func (d *daemon) rejectFn(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a message id, got %s", puff.Print(args[0]))
	}
	var reason puff.String
	if len(args) > 1 {
		if reason, ok = args[1].(puff.String); !ok {
			return nil, fmt.Errorf("want the reason as a string, got %s", puff.Print(args[1]))
		}
	}
	return d.reject(int64(id), string(reason)), nil
}

// clientReview accepts or rejects a message for a client: req.Args is the
// printed vector [id] for accept, [id reason] for reject. Its Value is the
// accepted message's result, or :done.
func (d *daemon) clientReview(ctx context.Context, req rpc.Request) rpc.Response {
	v, err := puff.ReadValue(req.Args)
	vec, ok := v.(puff.Vector)
	want := map[string]int{"accept": 1, "reject": 2}[req.Op]
	var id puff.Int
	var reason puff.String
	if err == nil && ok && vec.Len() == want {
		id, ok = vec.At(0).(puff.Int)
		if ok && want == 2 {
			reason, ok = vec.At(1).(puff.String)
		}
	}
	if err != nil || !ok || vec.Len() != want {
		return rpc.Response{Error: fmt.Sprintf("%s wants %s, got %s", req.Op, map[string]string{"accept": "[id]", "reject": "[id reason]"}[req.Op], req.Args)}
	}
	if req.Agent {
		return rpc.Response{Error: "an agent can't review its own messages"}
	}
	if req.Op == "reject" {
		return rpc.Response{Value: puff.Print(d.reject(int64(id), string(reason)))}
	}
	r, err := d.accept(ctx, nil, int64(id))
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	return rpc.Response{Value: puff.Print(r)}
}
