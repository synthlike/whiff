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

// message asks an address to run one of its functions. The arguments are a
// printed puff vector, so nothing but data travels.
type message struct {
	// ctx is the caller's, done when it stops waiting; nil for a send,
	// which the service bounds itself.
	ctx  context.Context
	fn   string
	args string
	dir  string // the caller's working directory
	// chain is the addresses waiting on calls that led to this one, so a
	// call back into one of them fails at once instead of timing out.
	chain []string
	// reply gets the printed Result, nil for a send. It has room for it, so
	// the service never waits on a caller that has gone.
	reply chan string
}

// caller is who whiff's builtins act for: a service handling a message, or
// a client's eval, which has no address.
type caller struct {
	addr  string
	chain []string // addresses waiting in the current chain, addr last
}

// route owns the address table until the daemon stops; others reach it
// through do.
func (d *daemon) route() {
	addrs := map[string]*service{}
	for {
		select {
		case op := <-d.ops:
			op(addrs)
		case <-d.ctx.Done():
			return
		}
	}
}

// do runs op on the route goroutine and waits for it. It reports false if
// the daemon is stopping and op didn't run.
func (d *daemon) do(op func(addrs map[string]*service)) bool {
	done := make(chan struct{})
	select {
	case d.ops <- func(addrs map[string]*service) { op(addrs); close(done) }:
	case <-d.ctx.Done():
		return false
	}
	<-done
	return true
}

// deliver puts m in addr's mailbox, or returns the (err e) for its sender.
func (d *daemon) deliver(addr string, m message) puff.Value {
	var res puff.Value
	ok := d.do(func(addrs map[string]*service) {
		s := addrs[addr]
		if s == nil {
			res = puff.Errorf("no-address", "no address :%s", addr)
			return
		}
		select {
		case s.mailbox <- m:
		default:
			res = puff.Errorf("full", ":%s's mailbox is full", addr)
		}
	})
	if !ok {
		return puff.Errorf("no-address", "whiff is stopping")
	}
	return res
}

// builtins are whiff's functions on top of puff's base table, acting for
// cur. All three are :write until marks follow the target.
func (d *daemon) builtins(cur *caller) []*puff.Builtin {
	return []*puff.Builtin{
		{Name: "call", Args: "target [arg] ...", Mark: puff.MarkWrite,
			Doc: "Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				return d.call(c, cur, args)
			}},
		{Name: "send", Args: "target [arg] ...", Mark: puff.MarkWrite,
			Doc: "Send target, :address/fn, a message without waiting: :done once it is in the mailbox.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := cur.message(c, args)
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
				m, addr, err := cur.message(c, args[1:])
				if err != nil {
					return nil, err
				}
				time.AfterFunc(time.Duration(after), func() {
					if d.ctx.Err() != nil {
						return
					}
					if e := d.deliver(addr, m); e != nil {
						fmt.Fprintf(d.log, "whiff: dropped a send-after to %s: %s\n", puff.Print(args[1]), puff.Print(e))
					}
				})
				return puff.Done, nil
			}},
	}
}

// call sends a message and waits for its reply. A call to an address
// already waiting in the chain would wait for itself, so it fails at once.
func (d *daemon) call(c *puff.Call, cur *caller, args []puff.Value) (puff.Value, error) {
	m, addr, err := cur.message(c, args)
	if err != nil {
		return nil, err
	}
	if slices.Contains(cur.chain, addr) {
		var path strings.Builder
		for _, a := range cur.chain {
			fmt.Fprintf(&path, ":%s -> ", a)
		}
		path.WriteString(":" + addr)
		return puff.Errorf("cycle", "%s would wait for itself: %s", puff.Print(args[0]), path.String()), nil
	}
	ctx, cancel := context.WithTimeout(c.Ctx, d.callTimeout)
	defer cancel()
	m.ctx = ctx
	m.chain = cur.chain
	m.reply = make(chan string, 1)
	if e := d.deliver(addr, m); e != nil {
		return e, nil
	}
	select {
	case r := <-m.reply:
		return puff.ReadValue(r)
	case <-ctx.Done():
		if c.Ctx.Err() != nil {
			return nil, errors.New("cancelled")
		}
		return puff.Errorf("timeout", "%s didn't reply within %v", puff.Print(args[0]), d.callTimeout), nil
	}
}

// message builds the message args, a target and its arguments, ask for,
// and returns it with the target's address.
func (cur *caller) message(c *puff.Call, args []puff.Value) (message, string, error) {
	addr, fn, err := cur.target(args[0])
	if err != nil {
		return message{}, "", err
	}
	msg, err := encode(puff.NewVector(args[1:]...))
	if err != nil {
		return message{}, "", err
	}
	return message{fn: fn, args: msg, dir: c.Env.Dir}, addr, nil
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
