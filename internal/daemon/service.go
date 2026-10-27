package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// loadTimeout bounds evaluating a service file; a var so tests can shorten
// it.
var loadTimeout = 5 * time.Second

// service is a loaded (srv name ...): its env belongs to the goroutine
// running it. Its name, fns, code and forms don't change after loading, so
// anyone can read them.
type service struct {
	name    string
	env     *puff.Env
	fns     map[string]handler
	subs    map[string]subscriber   // by topic
	code    map[string][]*puff.Form // the body or value of each handler and def
	forms   []*puff.Form            // srv's body, as written
	mailbox chan message
	cur     caller // whiff's builtins in env act for it
	d       *daemon
	quit    chan struct{} // closed to stop it after the message it is handling
	done    chan struct{} // closed when it has stopped
}

// subscriber is an (on :topic (v) body...): fn runs with each value
// published to topic.
type subscriber struct {
	pos   puff.Pos
	param string
	body  []*puff.Form
	fn    puff.Value
}

// handler is an (on (fn params...) [:read|:write] "doc" body...).
type handler struct {
	pos    puff.Pos
	params []string
	doc    string
	mark   puff.Mark // when marked: :read or :write after the parameters
	marked bool
	body   []*puff.Form
}

// loadService reads and evaluates a service file: one (srv name forms...),
// named like the file. Each on becomes a def, so the service calls its own
// functions by plain name.
func (d *daemon) loadService(file string) (*service, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	forms, err := puff.Read(file, string(src))
	if err != nil {
		return nil, err
	}
	if len(forms) != 1 || !isList(forms[0], "srv") {
		return nil, &puff.Error{Pos: puff.Pos{File: file, Line: 1, Col: 1}, Msg: "a service file holds one (srv name ...)"}
	}
	srv := forms[0]
	name := strings.TrimSuffix(filepath.Base(file), ".puff")
	if len(srv.Items) < 2 || srv.Items[1].Kind != puff.FormSymbol || srv.Items[1].Name != name {
		return nil, &puff.Error{Pos: srv.Pos, Msg: fmt.Sprintf("srv wants its name, %s, as in its file name", name)}
	}
	body := srv.Items[2:]
	if len(body) > 0 && body[0].Kind == puff.FormLiteral {
		if _, ok := body[0].Val.(puff.Keyword); ok {
			return nil, &puff.Error{Pos: body[0].Pos, Msg: "unknown srv option " + body[0].String()}
		}
	}

	s := &service{name: name, fns: map[string]handler{}, subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		forms: body, mailbox: make(chan message, mailboxSize), cur: caller{addr: name}, d: d,
		quit: make(chan struct{}), done: make(chan struct{})}
	var loads []*puff.Form
	fns := map[string]*puff.Form{} // each subscriber's (fn (v) body...)
	for _, f := range body {
		switch {
		case isList(f, "on") && len(f.Items) > 1 && f.Items[1].Kind == puff.FormLiteral:
			topic, fn, sub, err := subscriberDef(f)
			if err != nil {
				return nil, err
			}
			if other, ok := s.subs[topic]; ok {
				return nil, &puff.Error{Pos: f.Pos, Msg: fmt.Sprintf("already subscribed to :%s at %s", topic, other.pos)}
			}
			s.subs[topic] = sub
			fns[topic] = fn
		case isList(f, "on"):
			fn, def, h, err := handlerDef(f)
			if err != nil {
				return nil, err
			}
			s.fns[fn] = h
			s.code[fn] = h.body
			loads = append(loads, def)
		default:
			if name, code, ok := defCode(f); ok {
				s.code[name] = code
			}
			loads = append(loads, f)
		}
	}
	s.env = d.newEnv(d.log, &s.cur)
	ctx, cancel := context.WithTimeout(d.ctx, loadTimeout)
	defer cancel()
	if _, err := s.env.LoadForms(ctx, loads); err != nil {
		return nil, err
	}
	for topic, fn := range fns {
		v, err := s.env.LoadForms(ctx, []*puff.Form{fn})
		if err != nil {
			return nil, err
		}
		sub := s.subs[topic]
		sub.fn = v
		s.subs[topic] = sub
	}
	return s, nil
}

// subscriberDef reads (on :topic (v) body...) and returns its topic and
// body as (fn (v) body...).
func subscriberDef(f *puff.Form) (string, *puff.Form, subscriber, error) {
	bad := func(msg string) (string, *puff.Form, subscriber, error) {
		return "", nil, subscriber{}, &puff.Error{Pos: f.Pos, Msg: msg}
	}
	topic, ok := f.Items[1].Val.(puff.Keyword)
	if !ok || topic == "" {
		return bad("on wants :topic or (fn params...), got " + f.Items[1].String())
	}
	if len(f.Items) < 4 || f.Items[2].Kind != puff.FormList || len(f.Items[2].Items) != 1 || f.Items[2].Items[0].Kind != puff.FormSymbol {
		return bad("on :topic wants (v), one parameter for the value, and a body")
	}
	sub := subscriber{pos: f.Pos, param: f.Items[2].Items[0].Name, body: f.Items[3:]}
	items := append([]*puff.Form{{Kind: puff.FormSymbol, Pos: f.Items[0].Pos, Name: "fn"}, f.Items[2]}, sub.body...)
	return string(topic), &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, sub, nil
}

// handlerDef turns (on (fn params...) [:read|:write] "doc" body...) into
// (def (fn params...) body...).
func handlerDef(f *puff.Form) (string, *puff.Form, handler, error) {
	bad := func(msg string) (string, *puff.Form, handler, error) {
		return "", nil, handler{}, &puff.Error{Pos: f.Pos, Msg: msg}
	}
	if len(f.Items) < 2 {
		return bad(`on wants (fn params...), a doc string and a body`)
	}
	sig, rest := f.Items[1], f.Items[2:]
	if sig.Kind != puff.FormList || len(sig.Items) == 0 || sig.Items[0].Kind != puff.FormSymbol {
		return bad("on wants (fn params...)")
	}
	h := handler{pos: f.Pos}
	if len(rest) > 0 && rest[0].Kind == puff.FormLiteral {
		if k, ok := rest[0].Val.(puff.Keyword); ok {
			switch k {
			case "read":
				h.mark, h.marked = puff.MarkRead, true
			case "write":
				h.mark, h.marked = puff.MarkWrite, true
			default:
				return bad("on takes :read or :write after (fn params...), got " + rest[0].String())
			}
			rest = rest[1:]
		}
	}
	if len(rest) < 2 {
		return bad(`on wants (fn params...), a doc string and a body`)
	}
	doc := rest[0]
	h.body = rest[1:]
	if s, ok := doc.Val.(puff.String); doc.Kind == puff.FormLiteral && ok {
		h.doc = string(s)
	} else {
		return bad("on wants a doc string after (fn params...)")
	}
	for _, p := range sig.Items[1:] {
		if p.Kind != puff.FormSymbol {
			return bad("on's parameters are names")
		}
		h.params = append(h.params, p.Name)
	}
	items := append([]*puff.Form{{Kind: puff.FormSymbol, Pos: f.Items[0].Pos, Name: "def"}, sig}, h.body...)
	return sig.Items[0].Name, &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, h, nil
}

// defCode returns the name a (def name value) or (def (name params...)
// body...) defines, and its value or body.
func defCode(f *puff.Form) (string, []*puff.Form, bool) {
	if !isList(f, "def") || len(f.Items) < 3 {
		return "", nil, false
	}
	switch n := f.Items[1]; {
	case n.Kind == puff.FormSymbol:
		return n.Name, f.Items[2:], true
	case n.Kind == puff.FormList && len(n.Items) > 0 && n.Items[0].Kind == puff.FormSymbol:
		return n.Items[0].Name, f.Items[2:], true
	}
	return "", nil, false
}

func isList(f *puff.Form, head string) bool {
	return f.Kind == puff.FormList && len(f.Items) > 0 && f.Items[0].Kind == puff.FormSymbol && f.Items[0].Name == head
}

// run handles s's messages one at a time until it is told to quit or ctx
// ends. After a fault it asks the supervisor to restart it from its file.
func (s *service) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case m := <-s.mailbox:
			begin := time.Now()
			r, faulted := s.handle(ctx, m)
			// Recorded before the reply, so the caller finds it in history.
			s.d.do(func(st *state) {
				st.record(entry{at: begin, kind: m.kind(), from: m.from, to: m.to(s.name),
					args: m.args, reply: r, took: time.Since(begin)})
			})
			if m.reply != nil {
				m.reply <- r
			}
			if faulted {
				select {
				case s.d.restarts <- s:
				default: // the supervisor has restarts waiting; this one can wait for the next fault
				}
			}
		case <-s.quit:
			return
		case <-ctx.Done():
			return
		}
	}
}

// handle runs the function or subscriber m is for and returns its Result,
// printed. A fault in it replies (err {:kind :fault}) and reports faulted,
// unless it was the caller giving up.
func (s *service) handle(ctx context.Context, m message) (reply string, faulted bool) {
	fail := func(kind, format string, args ...any) (string, bool) {
		return puff.Print(puff.Errorf(kind, format, args...)), false
	}
	var f puff.Value
	var pos puff.Pos
	var n int
	what := ":" + m.to(s.name)
	if m.topic != "" {
		sub, ok := s.subs[m.topic]
		if !ok {
			return fail("no-function", ":%s isn't subscribed to :%s", s.name, m.topic)
		}
		f, pos, n = sub.fn, sub.pos, 1
	} else {
		h, ok := s.fns[m.fn]
		if !ok {
			return fail("no-function", ":%s has no function %s", s.name, m.fn)
		}
		f, _ = s.env.Lookup(m.fn)
		pos, n = h.pos, len(h.params)
	}
	v, err := puff.ReadValue(m.args)
	if err != nil {
		return fail("bad-call", "%v", err)
	}
	vec := v.(puff.Vector)
	if vec.Len() != n {
		return fail("bad-call", "%s takes %d arguments, got %d", what, n, vec.Len())
	}
	args := make([]puff.Value, vec.Len())
	for i := range args {
		args[i] = vec.At(i)
	}
	if m.ctx == nil {
		var cancel context.CancelFunc
		m.ctx, cancel = context.WithTimeout(ctx, s.d.callTimeout)
		defer cancel()
	}
	s.cur.chain = slices.Concat(m.chain, []string{s.name})
	s.env.Dir = m.dir
	r, err := s.env.Call(m.ctx, pos, f, args...)
	if err != nil {
		return puff.Print(puff.Errorf("fault", "%v", err)), m.ctx.Err() == nil
	}
	if t, ok := r.(puff.Tag); !ok || t.Len() != 1 || t.Name() != "ok" && t.Name() != "err" {
		return fail("fault", "%s: %s returned %s, want (ok v) or (err e)", pos, what, puff.Print(r))
	}
	reply, err = encode(r)
	if err != nil {
		return fail("fault", "%s: %s: %v", pos, what, err)
	}
	return reply, false
}
