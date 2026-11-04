package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/synthlike/whiff/internal/puff"
)

// marker works out the marks of services' functions from their source, over
// a snapshot of the address table. A function is :write if anything it
// reaches is: a :write builtin, a handler or def of its service by name, or
// the function a literal target of call, send or send-after names. A
// computed target, a target that doesn't exist, and call or send used as a
// value are :write, and so is an :update handler or subscriber: the state
// it changes is yours. proc/run, proc/output and a call or send to :jobs/run
// of a literal command in puff's table of read-only commands are :read. A
// handler marked :write is :write whatever it reaches. One marked :read is
// :read too: a claim, reviewed with its file, which is an override when it
// lowers what the handler reaches (gh's poll, which only caches). whiff's
// own functions have the marks they say.
type marker struct {
	services map[string]*service
	modes    map[string]string // agent sessions' modes, as ls shows them
	base     *puff.Env         // for builtins' marks, and an eval's earlier defs'
	// trusted is the services an eval's session trusts: a message to one
	// is :read, as it goes through as accepted.
	trusted map[string]bool

	seen map[string]bool // what this walk has visited: see visit
	// strict leaves out :read claims: what a function reaches whatever its
	// handlers say, for overrides and the backstop (see service.handle).
	strict bool
	// why is the first :write form found, for an override's warning.
	why *puff.Form
}

func (d *daemon) marker() *marker {
	mk := &marker{base: d.base}
	d.do(func(st *state) {
		mk.services = maps.Clone(st.addrs)
		mk.modes = map[string]string{}
		for addr := range st.modes {
			if m := st.modeOf(addr); m != nil {
				mk.modes[addr] = m.String()
			}
		}
	})
	return mk
}

// markerOn is a marker over the address table as it is, on the route
// goroutine.
func (d *daemon) markerOn(st *state) *marker {
	return &marker{base: d.base, services: st.addrs}
}

// visit says whether key, a handler or subscriber, is new to this walk,
// and marks it seen.
func (mk *marker) visit(key string) bool {
	if mk.seen[key] {
		return false
	}
	if mk.seen == nil {
		mk.seen = map[string]bool{}
	}
	mk.seen[key] = true
	return true
}

// mark is the mark of s's handler fn.
func (mk *marker) mark(s *service, fn string) puff.Mark {
	mk.seen, mk.strict, mk.why = nil, false, nil
	return mk.node(s, fn)
}

// reaches is the mark of s's handler fn, or of its subscriber to topic
// if fn is "", with every :read claim left out.
func (mk *marker) reaches(s *service, fn, topic string) puff.Mark {
	mk.seen, mk.strict, mk.why = nil, true, nil
	if fn == "" {
		return mk.sub(s, s.subs[topic])
	}
	return mk.node(s, fn)
}

// override says why s's handler fn is one: what :write thing its :read
// claim leaves out. ok is false if it isn't one.
func (mk *marker) override(s *service, fn string) (why string, ok bool) {
	h := s.fns[fn]
	if !h.marked || h.mark != puff.MarkRead || h.native != nil || mk.reaches(s, fn, "") == puff.MarkRead {
		return "", false
	}
	at := mk.why.Pos
	if at.File == h.pos.File {
		at.File = ""
	}
	return fmt.Sprintf("%s: :%s/%s is marked :read, but reaches %s at %s", h.pos, s.name, fn, mk.why, at), true
}

// write is :write, and notes f as why if it is the first :write form found.
func (mk *marker) write(f *puff.Form) puff.Mark {
	if mk.why == nil {
		mk.why = f
	}
	return puff.MarkWrite
}

// node is the mark of name, a handler or def of s. Recursion adds nothing,
// so a name already being worked out counts as :read.
func (mk *marker) node(s *service, name string) puff.Mark {
	h, ok := s.fns[name]
	switch {
	case ok && h.marked && (h.mark == puff.MarkWrite || !mk.strict || h.native != nil):
		return h.mark
	case ok && h.update:
		return mk.updates(h.pos)
	}
	if !mk.visit(s.name + "/" + name) {
		return puff.MarkRead
	}
	return mk.forms(s, s.code[name])
}

func (mk *marker) forms(s *service, fs []*puff.Form) puff.Mark {
	for _, f := range fs {
		if mk.form(s, f) == puff.MarkWrite {
			return puff.MarkWrite
		}
	}
	return puff.MarkRead
}

func (mk *marker) form(s *service, f *puff.Form) puff.Mark {
	switch f.Kind {
	case puff.FormSymbol:
		if _, ok := s.code[f.Name]; ok {
			if mk.node(s, f.Name) == puff.MarkWrite {
				return mk.write(f)
			}
			return puff.MarkRead
		}
		if v, ok := mk.base.Lookup(f.Name); ok {
			if puff.MarkOf(v) == puff.MarkWrite {
				return mk.write(f)
			}
		}
	case puff.FormList, puff.FormVector, puff.FormMap:
		if isList(f, "publish") && len(f.Items) > 1 {
			topic, ok := f.Items[1].Val.(puff.Keyword)
			if f.Items[1].Kind != puff.FormLiteral || !ok || mk.topic(string(topic)) == puff.MarkWrite {
				return mk.write(f.Items[0])
			}
			return mk.forms(s, f.Items[2:])
		}
		if (isList(f, "proc/run") || isList(f, "proc/output")) && len(f.Items) > 1 && readOnlyCommand(f.Items[1]) {
			if _, ok := s.code[f.Items[0].Name]; !ok {
				return mk.forms(s, f.Items[2:])
			}
		}
		i := targetAt(f)
		if i == 0 {
			return mk.forms(s, f.Items)
		}
		target, ok := mk.resolve(s, f.Items[i])
		if !ok || !mk.trusted[target.s.name] && mk.node(target.s, target.fn) == puff.MarkWrite &&
			!(target.s.fns[target.fn].cmd && len(f.Items) > i+1 && readOnlyCommand(f.Items[i+1])) {
			return mk.write(f.Items[i])
		}
		return mk.forms(s, slices.Concat(f.Items[1:i], f.Items[i+1:]))
	}
	return puff.MarkRead
}

// readOnlyCommand says whether f is a literal vector of strings in puff's
// table of read-only commands.
func readOnlyCommand(f *puff.Form) bool {
	if f.Kind != puff.FormVector {
		return false
	}
	argv := make([]puff.Value, len(f.Items))
	for i, it := range f.Items {
		if it.Kind != puff.FormLiteral {
			return false
		}
		argv[i] = it.Val
	}
	return puff.ReadOnlyCommand(puff.NewVector(argv...))
}

// msgMark is the mark of m, a message to s: its function's, or :read for a
// read-only command to a function that runs one, :jobs/run.
func (mk *marker) msgMark(s *service, m message) puff.Mark {
	if mk.mark(s, m.fn) == puff.MarkRead {
		return puff.MarkRead
	}
	if v, err := puff.ReadValue(m.args); err == nil && s.fns[m.fn].readsCmd(v.(puff.Vector)) {
		return puff.MarkRead
	}
	return puff.MarkWrite
}

// evalMark is the mark of forms, code evaluated in env for from (an
// address, "" for none), as if they were a service's: what their defs,
// env's earlier ones and the targets they name reach. A literal target in
// a service from's trusted mode lets through is :read.
func (d *daemon) evalMark(env *puff.Env, from string, forms []*puff.Form) puff.Mark {
	s := &service{name: from, code: map[string][]*puff.Form{}}
	for _, f := range forms {
		if name, body, ok := defCode(f); ok {
			s.code[name] = body
		}
	}
	mk := d.marker()
	mk.base = env
	d.do(func(st *state) {
		if m := st.modeOf(from); m != nil {
			mk.trusted = maps.Clone(m.trust)
		}
	})
	return mk.forms(s, forms)
}

// topic is :write if any service's subscriber to it reaches :write.
func (mk *marker) topic(name string) puff.Mark {
	for _, s := range mk.services {
		sub, ok := s.subs[name]
		if !ok || !mk.visit(s.name+" on :"+name) {
			continue
		}
		if mk.sub(s, sub) == puff.MarkWrite {
			return puff.MarkWrite
		}
	}
	return puff.MarkRead
}

// sub is the mark of s's subscriber sub.
func (mk *marker) sub(s *service, sub subscriber) puff.Mark {
	if sub.update {
		return mk.updates(sub.pos)
	}
	return mk.forms(s, sub.body)
}

// updates is :write, for an :update handler or subscriber at pos.
func (mk *marker) updates(pos puff.Pos) puff.Mark {
	return mk.write(&puff.Form{Kind: puff.FormLiteral, Pos: pos, Val: puff.Keyword("update")})
}

// targetAt returns where the target is in a call, send or send-after, or 0
// if f isn't one.
func targetAt(f *puff.Form) int {
	switch {
	case (isList(f, "call") || isList(f, "send")) && len(f.Items) > 1:
		return 1
	case isList(f, "send-after") && len(f.Items) > 2:
		return 2
	}
	return 0
}

type target struct {
	s  *service
	fn string
}

// resolve finds the handler a literal target in s's source names.
func (mk *marker) resolve(s *service, f *puff.Form) (target, bool) {
	k, ok := f.Val.(puff.Keyword)
	if f.Kind != puff.FormLiteral || !ok {
		return target{}, false
	}
	addr, fn, err := (&caller{addr: s.name}).target(k)
	if err != nil {
		return target{}, false
	}
	t, ok := mk.services[addr]
	if !ok {
		return target{}, false
	}
	_, ok = t.fns[fn]
	return target{t, fn}, ok
}

// missing lists the literal targets in s's source that don't exist.
func (mk *marker) missing(s *service) []string {
	var warnings []string
	var walk func(fs []*puff.Form)
	walk = func(fs []*puff.Form) {
		for _, f := range fs {
			if i := targetAt(f); i > 0 && f.Items[i].Kind == puff.FormLiteral {
				if _, ok := f.Items[i].Val.(puff.Keyword); ok {
					if _, ok := mk.resolve(s, f.Items[i]); !ok {
						warnings = append(warnings, fmt.Sprintf("%s: %s doesn't exist", f.Items[i].Pos, f.Items[i]))
					}
				}
			}
			walk(f.Items)
		}
	}
	walk(s.forms)
	return warnings
}

// describe is addr's interface as data: (ok [{:fn :params :doc :mark :form}
// ...]) sorted by name, with :yours #t on a function only your connections
// may use; or (err {:kind :no-address}).
func (mk *marker) describe(addr string) puff.Value {
	s, ok := mk.services[addr]
	if !ok {
		return puff.Errorf("no-address", "no address :%s", addr)
	}
	var fns []puff.Value
	for _, fn := range slices.Sorted(maps.Keys(s.fns)) {
		h := s.fns[fn]
		params := make([]puff.Value, len(h.params))
		for i, p := range h.params {
			params[i] = puff.Keyword(p)
		}
		form := "on"
		if h.ask {
			form = "ask"
		}
		es := []puff.Entry{
			puff.Key("fn", puff.Keyword(fn)),
			puff.Key("params", puff.NewVector(params...)),
			puff.Key("doc", puff.String(h.doc)),
			puff.Key("mark", puff.Keyword(strings.TrimPrefix(mk.mark(s, fn).String(), ":"))),
			puff.Key("form", puff.Keyword(form)),
		}
		if h.yours {
			es = append(es, puff.Key("yours", puff.Bool(true)))
		}
		m, _ := puff.NewMap(es...)
		fns = append(fns, m)
	}
	return puff.Ok(puff.NewVector(fns...))
}

// ls is every address's functions and subscriptions, then the read-only
// commands, the overrides, and the literal targets that don't exist:
//
//	:greet
//	  (hello name)        :read  Greets name.
//	  (status)            :read  Runs make status.
//	  ask (approve text)  :read  Waits for a person.
//	  on :git/changed (v) :read
//	read-only commands (internal/puff/commands.go): git status, git log, ...
//	override: greet.puff:6:3: :greet/status is marked :read, but reaches proc/run at 7:6
//	warning: greet.puff:4:11: :nobody/x doesn't exist
func (mk *marker) ls() string {
	var b strings.Builder
	var overrides, warnings []string
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, addr := range slices.Sorted(maps.Keys(mk.services)) {
		s := mk.services[addr]
		switch {
		case s.client && strings.HasPrefix(addr, "agent/"):
			m := mk.modes[addr]
			if m == "" {
				m = "gated"
			}
			fmt.Fprintf(tw, ":%s (agent, %s)\n", addr, m)
		case s.client:
			fmt.Fprintf(tw, ":%s (client)\n", addr)
		default:
			fmt.Fprintf(tw, ":%s\n", addr)
		}
		for _, fn := range slices.Sorted(maps.Keys(s.fns)) {
			h := s.fns[fn]
			params := slices.Clone(h.params)
			for i := len(params) - h.optional; i < len(params); i++ {
				params[i] = "[" + params[i] + "]"
			}
			sig := "(" + strings.Join(append([]string{fn}, params...), " ") + ")"
			if h.ask {
				sig = "ask " + sig
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", sig, mk.mark(s, fn), h.doc)
			if why, ok := mk.override(s, fn); ok {
				overrides = append(overrides, why)
			}
		}
		for _, topic := range slices.Sorted(maps.Keys(s.subs)) {
			sub := s.subs[topic]
			mk.seen, mk.strict = nil, false
			fmt.Fprintf(tw, "  on :%s (%s)\t%s\n", topic, sub.param, mk.forms(s, sub.body))
		}
		warnings = append(warnings, mk.missing(s)...)
	}
	tw.Flush()
	fmt.Fprintf(&b, "read-only commands (internal/puff/commands.go): %s\n", strings.Join(puff.ReadOnlyCommands(), ", "))
	for _, o := range overrides {
		fmt.Fprintf(&b, "override: %s\n", o)
	}
	for _, w := range warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}
