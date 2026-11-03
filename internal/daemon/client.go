package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// link is a client's connection, as the address it may hold sees it: its
// messages are pushed as Deliveries, and the client's answers come back on
// answers.
type link struct {
	conn    *rpc.Conn
	answers chan rpc.Request
	gone    chan struct{} // closed when the connection ends
	next    int64         // the last Delivery ID; the address's goroutine owns it
}

func newLink(conn *rpc.Conn) *link {
	return &link{conn: conn, answers: make(chan rpc.Request, 16), gone: make(chan struct{})}
}

// answered hands the client's answer to the delivery waiting for it, if
// any; a late one is dropped.
func (l *link) answered(req rpc.Request) {
	select {
	case l.answers <- req:
	default:
	}
}

// forward pushes m to the client and waits for its Result: the native
// handler of every function of a client's address.
func (l *link) forward(m message, _ []puff.Value) (puff.Value, error) {
	l.next++
	id := l.next
	what := ":" + m.fn
	err := l.conn.Send(rpc.Response{Delivery: &rpc.Delivery{
		ID: id, Fn: m.fn, Args: m.args, From: m.from, Origin: string(origin(m.user)), Dir: m.dir}})
	if err != nil {
		return puff.Errorf("no-address", "the client has gone"), nil
	}
	for {
		select {
		case a := <-l.answers:
			if a.ID != id {
				continue
			}
			r, err := puff.ReadValue(a.Args)
			if t, ok := r.(puff.Tag); err != nil || !ok || t.Len() != 1 || t.Name() != "ok" && t.Name() != "err" {
				return puff.Errorf("fault", "the client answered %s with %s, want (ok v) or (err e)", what, a.Args), nil
			}
			return r, nil
		case <-m.ctx.Done():
			return puff.Errorf("timeout", "the client didn't answer %s in time", what), nil
		case <-l.gone:
			return puff.Errorf("no-address", "the client has gone"), nil
		}
	}
}

// claim makes req.Target l's address, with the interface in req.Src. The
// Response's Value is (ok :done), or (err {:kind :taken}) for an address
// that is held, or that whiff assigns: :jobs, :me, :review, :agent/* and
// whiff try's :try/*. An agent's claim of :agent is given the next
// :agent/<n>, and its Value is (ok :agent/<n>).
func (d *daemon) claim(l *link, req rpc.Request) (*service, rpc.Response) {
	name := strings.TrimPrefix(req.Target, ":")
	if k, err := puff.ReadValue(":" + name); err != nil || name == "" || !puff.Equal(k, puff.Keyword(name)) {
		return nil, rpc.Response{Error: fmt.Sprintf("want an address like :hiss, got %s", req.Target)}
	}
	taken := func(why string) (*service, rpc.Response) {
		return nil, rpc.Response{Value: puff.Print(puff.Errorf("taken", ":%s %s", name, why))}
	}
	done := puff.Done
	var can map[string]bool
	if req.Token != "" {
		if name != "agent" || !req.Agent {
			return nil, rpc.Response{Error: "a session token claims :agent, for an agent"}
		}
		var g grant
		var ok bool
		d.do(func(st *state) { g, ok = st.grants[req.Token] })
		if !ok {
			return nil, rpc.Response{Value: puff.Print(puff.Errorf("no-session", "that session token isn't whiff's, or its agent's turn has ended"))}
		}
		name, can = g.session, g.can
		if name == "" {
			name = fmt.Sprintf("agent/%d", d.newAgent())
		}
		fns := slices.Sorted(maps.Keys(can))
		vals := make([]puff.Value, len(fns))
		for i, fn := range fns {
			vals[i] = puff.Keyword(fn)
		}
		done = puff.Ok(puff.NewVector(puff.Keyword(name), puff.NewVector(vals...)))
	} else if name == "agent" && req.Agent {
		name = fmt.Sprintf("agent/%d", d.newAgent())
		done = puff.Ok(puff.Keyword(name))
	} else if name == "jobs" || name == "me" || name == "review" || name == "agent" || strings.HasPrefix(name, "agent/") ||
		name == "try" || strings.HasPrefix(name, "try/") {
		return taken("is assigned by whiff")
	}
	fns, err := clientInterface(req.Src, l)
	if err != nil {
		return nil, rpc.Response{Error: err.Error()}
	}
	quit := make(chan struct{})
	s := &service{name: name, fns: fns, subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), cur: caller{addr: name, stop: quit}, d: d,
		quit: quit, done: make(chan struct{}), client: true, can: can}
	held := false
	d.do(func(st *state) {
		if held = st.addrs[name] != nil; !held {
			st.addrs[name] = s
		}
	})
	if held {
		return taken("is held")
	}
	d.wg.Go(func() { s.run(d.ctx) })
	return s, rpc.Response{Value: puff.Print(done)}
}

// clientInterface reads a client's (on (fn params...) [:read|:write] "doc")
// and (ask ...) forms, whose functions l forwards.
func clientInterface(src string, l *link) (map[string]handler, error) {
	forms, err := puff.Read("claim", src)
	if err != nil {
		return nil, err
	}
	fns := map[string]handler{}
	for _, f := range forms {
		var fn string
		var h handler
		switch {
		case isList(f, "on"):
			var rest []*puff.Form
			fn, h, rest, err = handlerHead(f, "on wants (fn params...) and a doc string", 1)
			switch {
			case err == nil && len(rest) > 0:
				err = &puff.Error{Pos: rest[0].Pos, Msg: "a client's on has no body"}
			case err == nil && h.update:
				err = &puff.Error{Pos: f.Pos, Msg: "a client has no state to :update"}
			}
			h.native = l.forward
		case isList(f, "ask"):
			fn, h, err = askDef(f)
		default:
			err = &puff.Error{Pos: f.Pos, Msg: "a client's interface is (on ...) and (ask ...) forms, got " + f.String()}
		}
		if err != nil {
			return nil, err
		}
		if other, ok := fns[fn]; ok {
			return nil, &puff.Error{Pos: f.Pos, Msg: fmt.Sprintf("%s is already defined at %s", fn, other.pos)}
		}
		fns[fn] = h
	}
	return fns, nil
}
