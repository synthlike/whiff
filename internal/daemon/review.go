package daemon

import (
	"context"
	"fmt"
	"io"
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
// a person anyway, nor a whiff/done, the answer to something s started.
func (d *daemon) gated(st *state, s *service, m message) bool {
	if m.user || m.reviewed || m.topic != "" || m.fn == "whiff/done" {
		return false
	}
	h, ok := s.fns[m.fn]
	if !ok || h.ask {
		return false
	}
	mk := &marker{base: d.base, services: st.addrs}
	return mk.mark(s, m.fn) == puff.MarkWrite
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
			if mk.topic(m.topic) == puff.MarkWrite {
				r = d.hold(st, "", m)
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
	a := &asked{ticket: ticket{from: m.from, dir: m.dir, user: m.user}, fn: m.fn, args: v.(puff.Vector), held: &held, to: addr}
	if m.topic != "" {
		a.fn, a.to = "publish", m.topic
	}
	return st.queue(a, a.what())
}

// holdEval puts an agent's code, req.Src, evaluated for from, in :review's
// inbox as {:fn :eval :args [src]}: see queue. Accepting runs it.
func (d *daemon) holdEval(st *state, from string, req rpc.Request) puff.Value {
	a := &asked{ticket: ticket{from: from, dir: req.Dir}, fn: "eval", args: puff.NewVector(puff.String(req.Src)), src: req.Src}
	return st.queue(a, "the agent's eval")
}

// queue puts a, which what names, in :review's inbox and publishes it on
// :review/pending, and returns the pending for its sender; or says why it
// can't wait there: nobody follows :review/pending to review it, or the
// inbox is full.
func (st *state) queue(a *asked, what string) puff.Value {
	if !st.reviewing() {
		return puff.Errorf("no-reviewer", "%s is :write, and nobody reviews agents' messages: a reviewer follows :review/pending (whiff sub review/pending)", what)
	}
	if len(st.inbox("review")) >= inboxSize {
		return puff.Errorf("full", ":review's inbox is full")
	}
	st.nextID++
	a.id, a.addr, a.at = st.nextID, "review", time.Now()
	st.asked = append(st.asked, a)
	st.publish(message{from: "review", topic: "review/pending", args: puff.Print(puff.NewVector(a.value())), user: a.user})
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
// ending.
func (d *daemon) accept(ctx context.Context, id int64) (puff.Value, error) {
	var a *asked
	d.do(func(st *state) { a = st.take("review", id) })
	if a == nil {
		return puff.Errorf("no-id", "no message %d waits for review", id), nil
	}
	if a.held == nil {
		r := d.runHeld(ctx, a)
		d.reviewed(a, r)
		return r, ctx.Err()
	}
	m := *a.held
	if m.topic != "" {
		m.reviewed = true
		d.publish(m)
		d.reviewed(a, puff.Done)
		return puff.Done, nil
	}
	m.reviewed = true
	r, err := d.request(ctx, &caller{}, a.to, m)
	if err != nil {
		r = puff.Errorf("cancelled", "the accept was cancelled before :%s replied", m.to(a.to))
	}
	d.reviewed(a, r)
	return r, err
}

// runHeld runs the code a holds, as its sender's eval would have and
// passing the gate, within the call timeout. Its Result is the code's
// value, (ok v) unless it is a Result, or a fault as (err {:kind :fault}).
// What it prints is dropped.
func (d *daemon) runHeld(ctx context.Context, a *asked) puff.Value {
	env := d.newEnv(io.Discard, &caller{addr: a.from, reviewed: true})
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
// :reason reason}) without delivering it.
func (d *daemon) reject(id int64, reason string) puff.Value {
	var a *asked
	d.do(func(st *state) { a = st.take("review", id) })
	if a == nil {
		return puff.Errorf("no-id", "no message %d waits for review", id)
	}
	e, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("kind"), Val: puff.Keyword("rejected")},
		puff.Entry{Key: puff.Keyword("message"), Val: puff.String(fmt.Sprintf("you rejected %s: %s", a.what(), reason))},
		puff.Entry{Key: puff.Keyword("reason"), Val: puff.String(reason)},
	)
	d.reviewed(a, puff.NewTag("err", e))
	return puff.Done
}

// reviewed answers a's sender with result, and publishes {:id :result} on
// :review/done.
func (d *daemon) reviewed(a *asked, result puff.Value) {
	done, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("id"), Val: puff.Int(a.id)},
		puff.Entry{Key: puff.Keyword("result"), Val: result},
	)
	d.do(func(st *state) {
		d.answer(st, a.ticket, a.to, result)
		st.publish(message{from: "review", topic: "review/done", args: puff.Print(puff.NewVector(done)), user: a.user})
	})
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
	r, err := d.accept(ctx, int64(id))
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	return rpc.Response{Value: puff.Print(r)}
}
