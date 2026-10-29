package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// inboxSize bounds each service's inbox.
const inboxSize = 100

// ticket is a message answered later, with [:whiff/done id result]: its
// id, and where the answer goes. The answer continues the message's chain,
// as :agent's if an agent gave it: see reply.
type ticket struct {
	id   int64
	from string // the sender's address; "" for a client
	dir  string // the sender's working directory, for the answer
	user bool   // whether the message's chain began with the user
	// reviewed is whether you gave the answer, so what it leads to passes
	// the review gate, as for a message you accepted.
	reviewed bool
	session  string // the agent session that began the message's chain, if one did
}

// asked is a message to an ask function, waiting in its service's inbox
// for a reply; or, in :review's, a message the review gate holds.
type asked struct {
	ticket
	addr string // the service whose inbox it is in
	fn   string
	args puff.Vector
	at   time.Time // when it arrived
	// held is the message :review holds, for to's fn, as it was sent; or
	// src is the code it holds, an agent's eval.
	held *message
	to   string
	src  string
}

// what names a held message, publish or eval: :todo/add, publish
// :git/changed, or the agent's eval.
func (a *asked) what() string {
	switch {
	case a.held == nil:
		return "the agent's eval"
	case a.held.topic != "":
		return "publish :" + a.held.topic
	}
	return ":" + a.held.to(a.to)
}

// pending is the (err {:kind :pending :id id}) a sender gets for a message
// answered later.
func pending(id int64, format string, args ...any) puff.Value {
	e := puff.Errorf("pending", format, args...).(puff.Tag).At(0).(puff.Map)
	e, _ = e.With(puff.Keyword("id"), puff.Int(id))
	return puff.NewTag("err", e)
}

// ask puts m, for an ask function of s, in s's inbox and returns what its
// sender gets at once: (err {:kind :pending :id n}), or why it isn't
// there. An ask with :expires is answered (err {:kind :expired}) once that
// has passed.
func (d *daemon) ask(st *state, s *service, m message) puff.Value {
	h := s.fns[m.fn]
	v, err := puff.ReadValue(m.args)
	args, _ := v.(puff.Vector)
	switch {
	case err != nil:
		return puff.Errorf("bad-call", "%v", err)
	case args.Len() > len(h.params) || args.Len() < len(h.params)-h.optional:
		want := fmt.Sprint(len(h.params))
		if h.optional > 0 {
			want = fmt.Sprintf("%d to %d", len(h.params)-h.optional, len(h.params))
		}
		return puff.Errorf("bad-call", ":%s takes %s arguments, got %d", m.to(s.name), want, args.Len())
	case len(st.inbox(s.name)) >= inboxSize:
		return puff.Errorf("full", ":%s's inbox is full", s.name)
	}
	if h.check != nil {
		if err := h.check(args); err != nil {
			return puff.Errorf("bad-call", ":%s: %v", m.to(s.name), err)
		}
	}
	st.nextID++
	a := &asked{ticket: ticket{id: st.nextID, from: m.from, dir: m.dir, user: m.user, session: m.session}, addr: s.name, fn: m.fn, args: args, at: time.Now()}
	st.asked = append(st.asked, a)
	if s.name == "me" {
		st.meChanged(m.user)
	}
	if h.expires > 0 {
		time.AfterFunc(h.expires, func() {
			if d.ctx.Err() != nil {
				return
			}
			d.do(func(st *state) {
				if st.take(a.addr, a.id) != nil {
					d.answer(st, a.ticket, a.addr, puff.Errorf("expired", ":%s waited longer than %v", m.to(a.addr), h.expires))
				}
			})
		})
	}
	return pending(a.id, ":%s waits in :%s's inbox for a reply", m.to(s.name), s.name)
}

// inbox is what waits in addr's inbox, oldest first.
func (st *state) inbox(addr string) []*asked {
	var in []*asked
	for _, a := range st.asked {
		if a.addr == addr {
			in = append(in, a)
		}
	}
	return in
}

// take removes the message id from addr's inbox, or from any inbox if addr
// is "", and returns it; nil if it isn't there. Taken from :me's, it
// publishes :me/changed.
func (st *state) take(addr string, id int64) *asked {
	i := slices.IndexFunc(st.asked, func(a *asked) bool { return a.id == id && (addr == "" || a.addr == addr) })
	if i < 0 {
		return nil
	}
	a := st.asked[i]
	st.asked = slices.Delete(st.asked, i, i+1)
	if a.addr == "me" {
		st.meChanged(a.user)
	}
	return a
}

// answered is the result a message or job was answered with, by, for its
// ticket.
type answered struct {
	ticket
	by     string
	result puff.Value
}

// answer tells t's sender its result, from by (an address, "" for a
// client): [:whiff/done id result] in its mailbox if it is a service with
// an (on (whiff/done id result) ...), else a line in history. Anyone in
// whiff wait for it gets it too, and so does a wait that comes later.
func (d *daemon) answer(st *state, t ticket, by string, result puff.Value) {
	st.answers = append(st.answers, answered{t, by, result})
	if n := len(st.answers) - historySize; n > 0 {
		st.answers = slices.Delete(st.answers, 0, n)
	}
	for _, w := range st.waiters[t.id] {
		w <- result
	}
	delete(st.waiters, t.id)
	m := message{from: by, fn: "whiff/done", args: puff.Print(puff.NewVector(puff.Int(t.id), result)), dir: t.dir, user: t.user, reviewed: t.reviewed, session: t.session}
	if s := st.addrs[t.from]; s != nil {
		if h, ok := s.fns[m.fn]; ok && !h.ask {
			d.put(st, t.from, m)
			return
		}
	}
	to := ""
	if t.from != "" {
		to = m.to(t.from)
	}
	st.record(entry{at: time.Now(), kind: m.kind(), user: m.user, from: by, to: to, args: m.args})
}

// handOn gives m's sender the answer to id, a job or ask by sent, as
// (hand-on r) does: so a handler can hand its caller a job it started, as
// if the caller had started it. The answer stays :user's only if both
// chains are. An answer that came before the hand-on goes to both. The
// error is for an id that isn't by's.
func (d *daemon) handOn(by string, id int64, m message) error {
	found := false
	d.do(func(st *state) {
		var t *ticket
		if j := st.jobs[id]; j != nil {
			t = &j.ticket
		} else if i := slices.IndexFunc(st.asked, func(a *asked) bool { return a.id == id }); i >= 0 {
			t = &st.asked[i].ticket
		}
		if t != nil && t.from == by {
			found = true
			t.from, t.dir, t.user, t.session = m.from, m.dir, t.user && m.user, sameSession(t.session, m.session)
			return
		}
		for i, a := range slices.Backward(st.answers) {
			if a.id == id && a.from == by {
				found = true
				// Handed on once: by can't hand it on again.
				st.answers[i].from = m.from
				d.answer(st, ticket{id: id, from: m.from, dir: m.dir, user: a.user && m.user, session: sameSession(a.session, m.session)}, a.by, a.result)
				return
			}
		}
	})
	if !found {
		return fmt.Errorf("no job or ask %d of :%s's to hand on", id, by)
	}
	return nil
}

// sameSession is a and b's agent session if they share it, else "".
func sameSession(a, b string) string {
	if a != b {
		return ""
	}
	return a
}

// pendingID is the id of r if it is (err {:kind :pending :id n}).
func pendingID(r puff.Value) (int64, bool) {
	t, ok := r.(puff.Tag)
	if !ok || t.Name() != "err" || t.Len() != 1 {
		return 0, false
	}
	e, ok := t.At(0).(puff.Map)
	if !ok {
		return 0, false
	}
	kind, _ := e.Get(puff.Keyword("kind"))
	id, _ := e.Get(puff.Keyword("id"))
	n, ok := id.(puff.Int)
	return int64(n), ok && puff.Equal(kind, puff.Keyword("pending"))
}

// reply answers the message id with result for by, a service that can
// answer only its own inbox, or a client ("") that can answer any. user is
// the replier's origin: the answer is :user's only if both the message and
// the reply are, so an agent's answer never runs as the user's. Yours
// passes the review gate all the same: you gave it.
func (d *daemon) reply(by string, user bool, idv, result puff.Value) (puff.Value, error) {
	id, ok := idv.(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a message id, got %s", puff.Print(idv))
	}
	if r, ok := result.(puff.Tag); !ok || r.Len() != 1 || r.Name() != "ok" && r.Name() != "err" {
		return nil, fmt.Errorf("want a Result, (ok v) or (err e), got %s", puff.Print(result))
	}
	if _, err := encode(result); err != nil {
		return nil, err
	}
	res := puff.Done
	d.do(func(st *state) {
		if slices.ContainsFunc(st.inbox("review"), func(a *asked) bool { return a.id == int64(id) }) {
			res = puff.Errorf("bad-call", "message %d waits for review: accept or reject it", id)
			return
		}
		a := st.take(by, int64(id))
		switch {
		case a == nil && by == "":
			res = puff.Errorf("no-id", "no message %d waits for a reply", id)
		case a == nil:
			res = puff.Errorf("no-id", "no message %d waits in :%s's inbox", id, by)
		default:
			t := a.ticket
			t.user, t.reviewed = t.user && user, user
			d.answer(st, t, by, result)
		}
	})
	return res, nil
}

// value is a as (inbox) shows it: {:id 7 :fn :suggest :args ["write docs"]
// :from :agent/1 :origin :agent :cwd "..." :at 2026-10-27T10:00:00Z},
// without :from for a client, and with :to, the address it is for, for a
// message the review gate holds.
func (a *asked) value() puff.Value {
	es := []puff.Entry{
		{Key: puff.Keyword("id"), Val: puff.Int(a.id)},
		{Key: puff.Keyword("fn"), Val: puff.Keyword(a.fn)},
		{Key: puff.Keyword("args"), Val: a.args},
		{Key: puff.Keyword("origin"), Val: origin(a.user)},
		{Key: puff.Keyword("cwd"), Val: puff.String(a.dir)},
		{Key: puff.Keyword("at"), Val: puff.NewTime(a.at)},
	}
	if a.from != "" {
		es = append(es, puff.Entry{Key: puff.Keyword("from"), Val: puff.Keyword(a.from)})
	}
	if a.held != nil {
		es = append(es, puff.Entry{Key: puff.Keyword("to"), Val: puff.Keyword(a.to)})
	}
	m, _ := puff.NewMap(es...)
	return m
}

// inboxText is addr's inbox, oldest first, a message a line:
//
//	7  client -> :todo/suggest "write docs"
//	8  :agent/1 -> eval "(file/write \"notes\" \"x\")"
//	9  :agent/1 -> publish :git/changed {:dir "..."}
func (d *daemon) inboxText(addr string) string {
	var b strings.Builder
	d.do(func(st *state) {
		for _, a := range st.inbox(addr) {
			from := "client"
			if a.from != "" {
				from = ":" + a.from
			}
			target := ":" + addr + "/" + a.fn
			switch {
			case a.src != "":
				target = "eval"
			case a.held != nil:
				target = a.what()
			}
			if args := puff.Print(a.args); args != "[]" {
				target += " " + args[1:len(args)-1]
			}
			fmt.Fprintf(&b, "%d  %s -> %s\n", a.id, from, target)
		}
	})
	return b.String()
}

// wait returns the answer to the message or job id, waiting for it while
// it is pending, or an (err {:kind :no-id}) if it is neither pending nor
// among the latest answers. The error is for ctx ending.
func (d *daemon) wait(ctx context.Context, id int64) (puff.Value, error) {
	var res puff.Value
	w := make(chan puff.Value, 1)
	d.do(func(st *state) {
		for _, a := range slices.Backward(st.answers) {
			if a.id == id {
				res = a.result
				return
			}
		}
		if st.jobs[id] == nil && !slices.ContainsFunc(st.asked, func(a *asked) bool { return a.id == id }) {
			res = puff.Errorf("no-id", "no message or job %d is pending or answered lately", id)
			return
		}
		st.waiters[id] = append(st.waiters[id], w)
	})
	if res != nil {
		return res, nil
	}
	select {
	case r := <-w:
		return r, nil
	case <-ctx.Done():
		d.do(func(st *state) {
			st.waiters[id] = slices.DeleteFunc(st.waiters[id], func(c chan puff.Value) bool { return c == w })
			if len(st.waiters[id]) == 0 {
				delete(st.waiters, id)
			}
		})
		return nil, errors.New("cancelled")
	}
}

// clientWait is wait for a client; req.Args is the printed vector [id].
func (d *daemon) clientWait(ctx context.Context, req rpc.Request) rpc.Response {
	v, err := puff.ReadValue(req.Args)
	vec, ok := v.(puff.Vector)
	var id puff.Int
	if err == nil && ok && vec.Len() == 1 {
		id, ok = vec.At(0).(puff.Int)
	}
	if err != nil || !ok || vec.Len() != 1 {
		return rpc.Response{Error: fmt.Sprintf("want [id], got %s", req.Args)}
	}
	r, err := d.wait(ctx, int64(id))
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	return rpc.Response{Value: puff.Print(r)}
}

// clientReply answers a message for a client; req.Args is the printed
// vector [id result]. Yours may answer any inbox; an agent's only that of
// the address it holds (from), so it can't answer a question to you.
func (d *daemon) clientReply(from string, req rpc.Request) rpc.Response {
	v, err := puff.ReadValue(req.Args)
	vec, ok := v.(puff.Vector)
	if err != nil || !ok || vec.Len() != 2 {
		return rpc.Response{Error: fmt.Sprintf("want [id result], got %s", req.Args)}
	}
	by := ""
	if req.Agent {
		if from == "" {
			return rpc.Response{Error: "an agent answers only its own inbox, and this connection holds no address"}
		}
		by = from
	}
	r, err := d.reply(by, !req.Agent, vec.At(0), vec.At(1))
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	return rpc.Response{Value: puff.Print(r)}
}
