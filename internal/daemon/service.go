package daemon

import (
	"context"
	"errors"
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

// service is a loaded (service name ...), or an address a client holds, whose
// functions are native and forward to it: its env belongs to the goroutine
// running it. Its name, fns, code and forms don't change after loading, so
// anyone can read them.
type service struct {
	name    string
	client  bool // held by a client connection
	env     *puff.Env
	fns     map[string]handler
	subs    map[string]subscriber   // by topic
	code    map[string][]*puff.Form // the body or value of each handler and def
	forms   []*puff.Form            // the service form's body, as written
	mailbox chan message
	cur     caller // whiff's builtins in env act for it
	d       *daemon
	// stateful is a service loaded with :state: its handlers take its
	// state as their first parameter, which callers don't pass. Its
	// goroutine owns state once it runs.
	stateful bool
	state    puff.Value
	keep     bool          // commit its state; not for whiff try's scratch addresses
	quit     chan struct{} // closed to stop it after the message it is handling
	done     chan struct{} // closed when it has stopped
}

// subscriber is an (on :topic (v) body...), or (on :topic (state v)
// [:update] body...) in a service with :state: fn runs with each value
// published to topic.
type subscriber struct {
	pos    puff.Pos
	param  string
	update bool
	body   []*puff.Form
	fn     puff.Value
}

// handler is an (on (fn params...) [:read|:write] [:update] "doc"
// body...), or an (ask (fn params...) [:read|:write] "doc" [:expires d]),
// which has no body: its messages wait in the inbox for a reply. In a
// service with :state, an on's first parameter is the state and isn't in
// params.
type handler struct {
	pos     puff.Pos
	params  []string
	doc     string
	mark    puff.Mark // when marked: :read or :write after the parameters
	marked  bool
	update  bool // :update: it returns the new state with its reply
	body    []*puff.Form
	ask     bool
	expires time.Duration // ask: how long a message waits for a reply; 0 for ever
	// native is a function of whiff's own services, in Go, instead of a
	// body. Its error replies (err {:kind :bad-call}).
	native   func(m message, args []puff.Value) (puff.Value, error)
	optional int // native: how many of the last params can be left out
	// cmd is for a native that runs its first argument, a command: :read
	// for one in puff's table of read-only commands.
	cmd bool
}

// readsCmd says whether h is :read for args, whatever its mark: they start
// with a read-only command that it runs.
func (h handler) readsCmd(args puff.Vector) bool {
	return h.cmd && args.Len() > 0 && puff.ReadOnlyCommand(args.At(0))
}

// loadService reads and evaluates a service file: one (service name forms...),
// named like the file. Each on becomes a def, so the service calls its own
// functions by plain name.
func (d *daemon) loadService(file string) (*service, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return d.loadSource(file, string(src), "", true)
}

// loadSource is loadService for src, read from file, at addr: the
// service's name if addr is "", else a scratch address for whiff try or
// test. Its :start runs if start is set.
func (d *daemon) loadSource(file, src, addr string, start bool) (*service, error) {
	forms, err := puff.Read(file, src)
	if err != nil {
		return nil, err
	}
	if len(forms) != 1 || !isList(forms[0], "service") {
		return nil, &puff.Error{Pos: puff.Pos{File: file, Line: 1, Col: 1}, Msg: "a service file holds one (service name ...)"}
	}
	srv := forms[0]
	name := strings.TrimSuffix(filepath.Base(file), ".puff")
	if len(srv.Items) < 2 || srv.Items[1].Kind != puff.FormSymbol || srv.Items[1].Name != name {
		return nil, &puff.Error{Pos: srv.Pos, Msg: fmt.Sprintf("service wants its name, %s, as in its file name", name)}
	}
	if name == "jobs" || name == "me" {
		return nil, &puff.Error{Pos: srv.Pos, Msg: ":" + name + " is whiff's own address; rename the file"}
	}
	body := srv.Items[2:]
	var startFn, init *puff.Form
	opts := map[puff.Keyword]struct {
		f    **puff.Form
		want string
	}{"start": {&startFn, ":start wants a function"}, "state": {&init, ":state wants its first state"}}
	for len(body) > 0 && body[0].Kind == puff.FormLiteral {
		k, ok := body[0].Val.(puff.Keyword)
		if !ok {
			break
		}
		opt, ok := opts[k]
		switch {
		case !ok:
			return nil, &puff.Error{Pos: body[0].Pos, Msg: "unknown service option " + body[0].String()}
		case *opt.f != nil:
			return nil, &puff.Error{Pos: body[0].Pos, Msg: "service takes " + body[0].String() + " once"}
		case len(body) < 2:
			return nil, &puff.Error{Pos: body[0].Pos, Msg: opt.want}
		}
		*opt.f, body = body[1], body[2:]
	}

	keep := addr == ""
	if addr == "" {
		addr = name
	}
	quit := make(chan struct{})
	s := &service{name: addr, fns: map[string]handler{}, subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		forms: srv.Items[2:], mailbox: make(chan message, mailboxSize), cur: caller{addr: addr, stop: quit}, d: d,
		quit: quit, done: make(chan struct{}), stateful: init != nil, keep: keep}
	var loads []*puff.Form
	fns := map[string]*puff.Form{} // each subscriber's (fn (v) body...)
	// add adds a handler named at pos, unless the name is taken.
	add := func(fn string, h handler, pos puff.Pos) error {
		if other, ok := s.fns[fn]; ok {
			return &puff.Error{Pos: pos, Msg: fmt.Sprintf("%s is already defined at %s", fn, other.pos)}
		}
		s.fns[fn] = h
		return nil
	}
	for _, f := range body {
		switch {
		case isList(f, "on") && len(f.Items) > 1 && f.Items[1].Kind == puff.FormLiteral:
			topic, fn, sub, err := subscriberDef(f, s.stateful)
			if err != nil {
				return nil, err
			}
			if other, ok := s.subs[topic]; ok {
				return nil, &puff.Error{Pos: f.Pos, Msg: fmt.Sprintf("already subscribed to :%s at %s", topic, other.pos)}
			}
			s.subs[topic] = sub
			fns[topic] = fn
		case isList(f, "on"):
			fn, def, h, err := handlerDef(f, s.stateful)
			if err != nil {
				return nil, err
			}
			if err := add(fn, h, f.Items[1].Items[0].Pos); err != nil {
				return nil, err
			}
			s.code[fn] = h.body
			loads = append(loads, def)
		case isList(f, "ask"):
			fn, h, err := askDef(f)
			if err != nil {
				return nil, err
			}
			if err := add(fn, h, f.Items[1].Items[0].Pos); err != nil {
				return nil, err
			}
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
	if init != nil {
		if err := s.initState(ctx, init); err != nil {
			return nil, err
		}
	}
	if start && startFn != nil {
		if err := s.start(ctx, startFn); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// initState gives s its state: the last one committed under its name, else
// init's value.
func (s *service) initState(ctx context.Context, init *puff.Form) error {
	if s.keep {
		p, found, err := s.d.storedState(s.name)
		if err != nil {
			return err
		}
		if found {
			v, err := puff.ReadValue(p)
			if err != nil {
				return fmt.Errorf(":%s's stored state: %v; whiff reset %s starts it afresh", s.name, err, s.name)
			}
			s.state = v
			return nil
		}
	}
	v, err := s.env.LoadForms(ctx, []*puff.Form{init})
	if err != nil {
		return err
	}
	if v == nil {
		return &puff.Error{Pos: init.Pos, Msg: ":state wants its first state"}
	}
	if err := s.commit(v); err != nil {
		return &puff.Error{Pos: init.Pos, Msg: ":state: " + err.Error()}
	}
	return nil
}

// commit makes v s's state, once it is stored: only data can be.
func (s *service) commit(v puff.Value) error {
	p := puff.Print(v)
	if _, err := puff.ReadValue(p); err != nil {
		return fmt.Errorf("can't keep %s as state: only data can be", p)
	}
	if s.keep {
		if err := s.d.storeState(s.name, p); err != nil {
			return fmt.Errorf("can't store the state: %v", err)
		}
	}
	s.state = v
	return nil
}

// update commits v, what an :update handler returned in its ok, and
// returns the reply. v is [reply state], or only the state when nobody
// waits for a reply (a subscriber, whiff/done), which then is (ok :done).
func (s *service) update(v puff.Value, alone bool) (puff.Value, error) {
	reply := puff.Done
	if !alone {
		vec, ok := v.(puff.Vector)
		if !ok || vec.Len() != 2 {
			return nil, fmt.Errorf("an :update handler returns (ok [reply state]) or (err e), got (ok %s)", puff.Print(v))
		}
		reply, v = puff.Ok(vec.At(0)), vec.At(1)
	}
	if err := s.commit(v); err != nil {
		return nil, err
	}
	return reply, nil
}

// start runs :start's function, f, as the user, before s takes its
// address: so a first message to itself goes with send-after. In a service
// with :state it takes the state and returns (ok state). An err, or
// anything but a Result, fails the load.
func (s *service) start(ctx context.Context, f *puff.Form) error {
	fn, err := s.env.LoadForms(ctx, []*puff.Form{f})
	if err != nil {
		return err
	}
	if fn == nil {
		return &puff.Error{Pos: f.Pos, Msg: ":start wants a function"}
	}
	s.cur.chain, s.cur.user = []string{s.name}, true
	var args []puff.Value
	want := "(ok v)"
	if s.stateful {
		args, want = []puff.Value{s.state}, "(ok state)"
	}
	r, err := s.env.Call(ctx, f.Pos, fn, args...)
	if err != nil {
		return err
	}
	t, ok := r.(puff.Tag)
	if !ok || t.Len() != 1 || t.Name() != "ok" {
		return &puff.Error{Pos: f.Pos, Msg: fmt.Sprintf(":start returned %s, want %s", puff.Print(r), want)}
	}
	if s.stateful {
		if err := s.commit(t.At(0)); err != nil {
			return &puff.Error{Pos: f.Pos, Msg: ":start: " + err.Error()}
		}
	}
	return nil
}

// subscriberDef reads (on :topic (v) body...), or (on :topic (state v)
// [:update] body...) if stateful, and returns its topic and body as (fn
// (v) body...).
func subscriberDef(f *puff.Form, stateful bool) (string, *puff.Form, subscriber, error) {
	bad := func(msg string) (string, *puff.Form, subscriber, error) {
		return "", nil, subscriber{}, &puff.Error{Pos: f.Pos, Msg: msg}
	}
	topic, ok := f.Items[1].Val.(puff.Keyword)
	if !ok || topic == "" {
		return bad("on wants :topic or (fn params...), got " + f.Items[1].String())
	}
	n, want := 1, "on :topic wants (v), one parameter for the value, and a body"
	if stateful {
		n, want = 2, "on :topic in a service with :state wants (state v) and a body"
	}
	if len(f.Items) < 4 || f.Items[2].Kind != puff.FormList || len(f.Items[2].Items) != n {
		return bad(want)
	}
	for _, p := range f.Items[2].Items {
		if p.Kind != puff.FormSymbol {
			return bad(want)
		}
	}
	sub := subscriber{pos: f.Pos, param: f.Items[2].Items[n-1].Name, body: f.Items[3:]}
	if k, ok := sub.body[0].Val.(puff.Keyword); sub.body[0].Kind == puff.FormLiteral && ok && k == "update" {
		if !stateful {
			return bad(":update needs a service with :state")
		}
		sub.update, sub.body = true, sub.body[1:]
		if len(sub.body) == 0 {
			return bad(want)
		}
	}
	items := append([]*puff.Form{{Kind: puff.FormSymbol, Pos: f.Items[0].Pos, Name: "fn"}, f.Items[2]}, sub.body...)
	return string(topic), &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, sub, nil
}

// handlerDef turns (on (fn params...) [:read|:write] [:update] "doc"
// body...) into (def (fn params...) body...). If stateful, the first
// parameter is the state.
func handlerDef(f *puff.Form, stateful bool) (string, *puff.Form, handler, error) {
	fn, h, body, err := handlerHead(f, "on wants (fn params...), a doc string and a body", 2)
	if err != nil {
		return "", nil, handler{}, err
	}
	switch {
	case h.update && !stateful:
		return "", nil, handler{}, &puff.Error{Pos: f.Pos, Msg: ":update needs a service with :state"}
	case stateful && len(h.params) == 0:
		return "", nil, handler{}, &puff.Error{Pos: f.Pos, Msg: "on in a service with :state wants (fn state params...)"}
	case stateful:
		h.params = h.params[1:]
	}
	h.body = body
	items := append([]*puff.Form{{Kind: puff.FormSymbol, Pos: f.Items[0].Pos, Name: "def"}, f.Items[1]}, h.body...)
	return fn, &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, h, nil
}

// askDef reads (ask (fn params...) [:read|:write] "doc" [:expires d]).
func askDef(f *puff.Form) (string, handler, error) {
	fn, h, opts, err := handlerHead(f, "ask wants (fn params...) and a doc string", 1)
	if err != nil {
		return "", handler{}, err
	}
	if h.update {
		return "", handler{}, &puff.Error{Pos: f.Pos, Msg: "an ask has no state to :update"}
	}
	h.ask = true
	for len(opts) > 0 {
		if k, ok := opts[0].Val.(puff.Keyword); opts[0].Kind != puff.FormLiteral || !ok || k != "expires" || len(opts) < 2 {
			return "", handler{}, &puff.Error{Pos: opts[0].Pos, Msg: "ask takes :expires d after its doc string, got " + opts[0].String()}
		}
		d, ok := opts[1].Val.(puff.Duration)
		if opts[1].Kind != puff.FormLiteral || !ok || d <= 0 {
			return "", handler{}, &puff.Error{Pos: opts[1].Pos, Msg: "want a positive :expires duration, got " + opts[1].String()}
		}
		h.expires = time.Duration(d)
		opts = opts[2:]
	}
	return fn, h, nil
}

// handlerHead reads what on and ask (f's head) start with: (fn params...)
// [:read|:write] [:update] "doc", the marks in any order, and returns the
// forms after the doc. want is the error for fewer than min forms after the
// marks, the doc included.
func handlerHead(f *puff.Form, want string, min int) (string, handler, []*puff.Form, error) {
	head := f.Items[0].Name
	bad := func(msg string) (string, handler, []*puff.Form, error) {
		return "", handler{}, nil, &puff.Error{Pos: f.Pos, Msg: msg}
	}
	if len(f.Items) < 2 {
		return bad(want)
	}
	sig, rest := f.Items[1], f.Items[2:]
	if sig.Kind != puff.FormList || len(sig.Items) == 0 || sig.Items[0].Kind != puff.FormSymbol {
		return bad(head + " wants (fn params...)")
	}
	h := handler{pos: f.Pos}
	for len(rest) > 0 && rest[0].Kind == puff.FormLiteral {
		k, ok := rest[0].Val.(puff.Keyword)
		if !ok {
			break
		}
		switch {
		case k == "read" && !h.marked:
			h.mark, h.marked = puff.MarkRead, true
		case k == "write" && !h.marked:
			h.mark, h.marked = puff.MarkWrite, true
		case k == "update" && !h.update:
			h.update = true
		default:
			return bad(head + " takes :read or :write, and :update, after (fn params...), got " + rest[0].String())
		}
		rest = rest[1:]
	}
	if len(rest) < min {
		return bad(want)
	}
	doc := rest[0]
	if s, ok := doc.Val.(puff.String); doc.Kind == puff.FormLiteral && ok {
		h.doc = string(s)
	} else {
		return bad(head + " wants a doc string after (fn params...)")
	}
	for _, p := range sig.Items[1:] {
		if p.Kind != puff.FormSymbol {
			return bad(head + "'s parameters are names")
		}
		h.params = append(h.params, p.Name)
	}
	return sig.Items[0].Name, h, rest[1:], nil
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
				st.record(entry{at: begin, kind: m.kind(), user: m.user, from: m.from, to: m.to(s.name),
					args: m.args, reply: r, took: time.Since(begin), gate: m.gate})
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
	var native func(message, []puff.Value) (puff.Value, error)
	var pos puff.Pos
	var n, optional int
	var update bool
	what := ":" + m.to(s.name)
	if m.topic != "" {
		sub, ok := s.subs[m.topic]
		if !ok {
			return fail("no-function", ":%s isn't subscribed to :%s", s.name, m.topic)
		}
		f, pos, n, update = sub.fn, sub.pos, 1, sub.update
	} else {
		h, ok := s.fns[m.fn]
		if !ok || h.ask {
			// An ask's messages go to the inbox; this one came before a
			// reload turned an on into an ask.
			return fail("no-function", ":%s has no function %s", s.name, m.fn)
		}
		if native = h.native; native == nil {
			f, _ = s.env.Lookup(m.fn)
		}
		pos, n, optional, update = h.pos, len(h.params), h.optional, h.update
	}
	v, err := puff.ReadValue(m.args)
	if err != nil {
		return fail("bad-call", "%v", err)
	}
	vec := v.(puff.Vector)
	if l := vec.Len(); l > n || l < n-optional {
		want := fmt.Sprint(n)
		if optional > 0 {
			want = fmt.Sprintf("%d to %d", n-optional, n)
		}
		return fail("bad-call", "%s takes %s arguments, got %d", what, want, l)
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
	if native != nil {
		r, err := native(m, args)
		if err != nil {
			return fail("bad-call", "%s: %v", what, err)
		}
		return puff.Print(r), false
	}
	// The backstop: what a handler reaches, leaving out :read claims, sets
	// its mark at run time, so if it is :read, a :write builtin or target
	// it reaches all the same faults, whoever sent the message. One that
	// counts on a :read claim, an override, is :write here.
	m.ctx = puff.WithMark(m.ctx, what, s.d.marker().reaches(s, m.fn, m.topic))
	s.cur.chain = slices.Concat(m.chain, []string{s.name})
	s.cur.user, s.cur.reviewed = m.user, m.reviewed
	s.cur.handOn = func(id int64) error {
		if m.reply == nil {
			return errors.New("only a call's handler can hand on: nobody waits for this message's answer")
		}
		return s.d.handOn(s.name, id, m)
	}
	defer func() { s.cur.handOn = nil }()
	s.env.Dir = m.dir
	if s.stateful {
		args = append([]puff.Value{s.state}, args...)
	}
	r, err := s.env.Call(m.ctx, pos, f, args...)
	if err != nil {
		return puff.Print(puff.Errorf("fault", "%v", err)), m.ctx.Err() == nil
	}
	t, ok := r.(puff.Tag)
	if !ok || t.Len() != 1 || t.Name() != "ok" && t.Name() != "err" {
		return fail("fault", "%s: %s returned %s, want (ok v) or (err e)", pos, what, puff.Print(r))
	}
	// An :update handler's state is committed before its reply is sent.
	if update && t.Name() == "ok" {
		if r, err = s.update(t.At(0), m.topic != "" || m.fn == "whiff/done"); err != nil {
			return fail("fault", "%s: %s: %v", pos, what, err)
		}
	}
	reply, err = encode(r)
	if err != nil {
		return fail("fault", "%s: %s: %v", pos, what, err)
	}
	return reply, false
}
