package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
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
	// reply gets the printed Result, nil for a send. It has room for it, so
	// the service never waits on a caller that has gone.
	reply chan string
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
	chain []string // addresses waiting in the current chain, addr last
	user  bool     // whether the current chain began with the user
	// stop is closed when the service version acting stops; nil for a
	// client.
	stop <-chan struct{}
}

// state is what the route goroutine owns.
type state struct {
	addrs   map[string]*service
	history []entry  // oldest first
	asked   []*asked // every service's inbox, oldest first
	jobs    map[int64]*job
	nextID  int64                       // the last id given to a message answered later, or a job
	answers []answered                  // the latest answers, oldest first
	waiters map[int64][]chan puff.Value // whiff waits, by the id they wait for
}

// route owns the state until the daemon stops; others reach it through do.
func (d *daemon) route() {
	st := &state{addrs: map[string]*service{}, jobs: map[int64]*job{}, waiters: map[int64][]chan puff.Value{}}
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

// deliver puts m in addr's mailbox, or its inbox for an ask function, or
// returns the (err e) for its sender and records it in history.
func (d *daemon) deliver(addr string, m message) puff.Value {
	var res puff.Value
	if !d.do(func(st *state) { res = d.put(st, addr, m) }) {
		return puff.Errorf("no-address", "whiff is stopping")
	}
	return res
}

// put is deliver on the route goroutine. A message to an ask function
// goes to the inbox and is answered (err {:kind :pending :id n}) at once.
func (d *daemon) put(st *state, addr string, m message) puff.Value {
	var res puff.Value
	switch s := st.addrs[addr]; {
	case s == nil:
		res = puff.Errorf("no-address", "no address :%s", addr)
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
	st.record(entry{at: time.Now(), kind: m.kind(), user: m.user, from: m.from, to: m.to(addr), args: m.args, reply: puff.Print(res)})
	return res
}

// publish puts m, for a topic, in the mailbox of every service subscribed to
// it. A full mailbox is recorded in history; the publisher doesn't wait.
func (d *daemon) publish(m message) {
	d.do(func(st *state) {
		for addr, s := range st.addrs {
			if _, ok := s.subs[m.topic]; !ok {
				continue
			}
			select {
			case s.mailbox <- m:
			default:
				res := puff.Errorf("full", ":%s's mailbox is full", addr)
				st.record(entry{at: time.Now(), kind: m.kind(), user: m.user, from: m.from, to: m.to(addr), args: m.args, reply: puff.Print(res)})
			}
		}
	})
}

// builtins are whiff's functions on top of puff's base table, acting for
// cur. call, send and send-after are :write: in a handler, one with a
// literal target counts as its target's mark instead (see marker).
func (d *daemon) builtins(cur *caller) []*puff.Builtin {
	return []*puff.Builtin{
		{Name: "describe", Args: "address", Mark: puff.MarkRead,
			Doc: "(ok fns): address's functions, each {:fn :params :doc :mark :form}.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, ok := args[0].(puff.Keyword)
				if !ok {
					return nil, fmt.Errorf("want an address like :gh, got %s", puff.Print(args[0]))
				}
				return d.marker().describe(string(addr)), nil
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
		{Name: "reply", Args: "id result", Mark: puff.MarkWrite,
			Doc: "Answer the message id in this service's inbox with result: its sender gets [:whiff/done id result]. (err {:kind :no-id}) if it isn't there.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				return d.reply(cur.addr, cur.user, args[0], args[1])
			}},
		{Name: "publish", Args: "topic v", Mark: puff.MarkWrite,
			Doc: "Send v to every service subscribed to topic, a keyword: :done at once.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				topic, ok := args[0].(puff.Keyword)
				if !ok || topic == "" {
					return nil, fmt.Errorf("want a topic like :git/changed, got %s", puff.Print(args[0]))
				}
				msg, err := encode(puff.NewVector(args[1]))
				if err != nil {
					return nil, err
				}
				d.publish(message{from: cur.addr, topic: string(topic), args: msg, dir: c.Env.Dir, user: cur.user})
				return puff.Done, nil
			}},
		{Name: "call", Args: "target [arg] ...", Mark: puff.MarkWrite,
			Doc: "Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := cur.message(c.Env.Dir, args)
				if err != nil {
					return nil, err
				}
				return d.request(c.Ctx, cur, addr, m)
			}},
		{Name: "send", Args: "target [arg] ...", Mark: puff.MarkWrite,
			Doc: "Send target, :address/fn, a message without waiting: :done once it is in the mailbox.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := cur.message(c.Env.Dir, args)
				if err != nil {
					return nil, err
				}
				if e := d.deliver(addr, m); e != nil {
					return e, nil
				}
				return puff.Done, nil
			}},
		{Name: "send-after", Args: "d target [arg] ...", Mark: puff.MarkWrite,
			Doc: "Send target a message once d has passed; :done at once.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				after, ok := args[0].(puff.Duration)
				if !ok || after < 0 {
					return nil, fmt.Errorf("want a duration of 0s or more, got %s", puff.Print(args[0]))
				}
				m, addr, err := cur.message(c.Env.Dir, args[1:])
				if err != nil {
					return nil, err
				}
				// A service's timers to itself go with the version that set
				// them, so a reload, whose :start sets its own, doesn't
				// double a loop. A delivery that fails is in history.
				own, stop := addr == cur.addr, cur.stop
				time.AfterFunc(time.Duration(after), func() {
					select {
					case <-stop:
						if own {
							return
						}
					default:
					}
					if d.ctx.Err() == nil {
						d.deliver(addr, m)
					}
				})
				return puff.Done, nil
			}},
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
	return message{from: cur.addr, fn: fn, args: msg, dir: dir, user: cur.user}, addr, nil
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
