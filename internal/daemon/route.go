package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// mailboxSize bounds each address's mailbox.
const mailboxSize = 100

// message asks an address to run one of its functions. The arguments are a
// printed puff vector, so nothing but data travels.
type message struct {
	ctx  context.Context // the caller's: done when it stops waiting
	fn   string
	args string
	dir  string // the caller's working directory
	// reply gets the printed Result. It has room for it, so the service
	// never waits on a caller that has gone.
	reply chan string
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

// builtins are whiff's functions on top of puff's base table.
func (d *daemon) builtins() []*puff.Builtin {
	return []*puff.Builtin{
		// :write until marks follow the target.
		{Name: "call", Args: "target [arg] ...", Mark: puff.MarkWrite, Fn: d.call,
			Doc: "Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s."},
	}
}

func (d *daemon) call(c *puff.Call, args []puff.Value) (puff.Value, error) {
	addr, fn, err := target(args[0])
	if err != nil {
		return nil, err
	}
	msg, err := encode(puff.NewVector(args[1:]...))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Ctx, d.callTimeout)
	defer cancel()
	reply := make(chan string, 1)
	if e := d.deliver(addr, message{ctx: ctx, fn: fn, args: msg, dir: c.Env.Dir, reply: reply}); e != nil {
		return e, nil
	}
	select {
	case r := <-reply:
		return puff.ReadValue(r)
	case <-ctx.Done():
		if c.Ctx.Err() != nil {
			return nil, errors.New("cancelled")
		}
		return puff.Errorf("timeout", "%s didn't reply within %v", puff.Print(args[0]), d.callTimeout), nil
	}
}

// target splits :address/fn at its last slash: :gh/prs, :agent/1/inbox.
func target(v puff.Value) (addr, fn string, err error) {
	k, ok := v.(puff.Keyword)
	i := strings.LastIndexByte(string(k), '/')
	if !ok || i <= 0 || i == len(k)-1 {
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
