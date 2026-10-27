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
// value are :write. A handler marked :read or :write has that mark, whatever
// it reaches.
type marker struct {
	services map[string]*service
	base     *puff.Env // for builtins' marks
	seen     map[string]bool
}

func (d *daemon) marker() *marker {
	mk := &marker{base: d.base}
	d.do(func(st *state) { mk.services = maps.Clone(st.addrs) })
	return mk
}

// mark is the mark of s's handler fn.
func (mk *marker) mark(s *service, fn string) puff.Mark {
	mk.seen = map[string]bool{}
	return mk.node(s, fn)
}

// node is the mark of name, a handler or def of s. Recursion adds nothing,
// so a name already being worked out counts as :read.
func (mk *marker) node(s *service, name string) puff.Mark {
	if h, ok := s.fns[name]; ok && h.marked {
		return h.mark
	}
	key := s.name + "/" + name
	if mk.seen[key] {
		return puff.MarkRead
	}
	mk.seen[key] = true
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
			return mk.node(s, f.Name)
		}
		if v, ok := mk.base.Lookup(f.Name); ok {
			if b, ok := v.(*puff.Builtin); ok {
				return b.Mark
			}
		}
	case puff.FormList, puff.FormVector, puff.FormMap:
		if isList(f, "publish") && len(f.Items) > 1 {
			topic, ok := f.Items[1].Val.(puff.Keyword)
			if f.Items[1].Kind != puff.FormLiteral || !ok || mk.topic(string(topic)) == puff.MarkWrite {
				return puff.MarkWrite
			}
			return mk.forms(s, f.Items[2:])
		}
		i := targetAt(f)
		if i == 0 {
			return mk.forms(s, f.Items)
		}
		target, ok := mk.resolve(s, f.Items[i])
		if !ok || mk.node(target.s, target.fn) == puff.MarkWrite {
			return puff.MarkWrite
		}
		return mk.forms(s, slices.Concat(f.Items[1:i], f.Items[i+1:]))
	}
	return puff.MarkRead
}

// topic is :write if any service's subscriber to it reaches :write.
func (mk *marker) topic(name string) puff.Mark {
	for _, s := range mk.services {
		sub, ok := s.subs[name]
		key := s.name + " on :" + name
		if !ok || mk.seen[key] {
			continue
		}
		mk.seen[key] = true
		if mk.forms(s, sub.body) == puff.MarkWrite {
			return puff.MarkWrite
		}
	}
	return puff.MarkRead
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
// ...]) sorted by name, or (err {:kind :no-address}).
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
		m, _ := puff.NewMap(
			puff.Entry{Key: puff.Keyword("fn"), Val: puff.Keyword(fn)},
			puff.Entry{Key: puff.Keyword("params"), Val: puff.NewVector(params...)},
			puff.Entry{Key: puff.Keyword("doc"), Val: puff.String(h.doc)},
			puff.Entry{Key: puff.Keyword("mark"), Val: puff.Keyword(strings.TrimPrefix(mk.mark(s, fn).String(), ":"))},
			puff.Entry{Key: puff.Keyword("form"), Val: puff.Keyword(form)},
		)
		fns = append(fns, m)
	}
	return puff.Ok(puff.NewVector(fns...))
}

// ls is every address's functions and subscriptions, then the literal
// targets that don't exist:
//
//	:greet
//	  (hello name)        :read  Greets name.
//	  ask (approve text)  :read  Waits for a person.
//	  on :git/changed (v) :read
//	warning: greet.puff:4:11: :nobody/x doesn't exist
func (mk *marker) ls() string {
	var b strings.Builder
	var warnings []string
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, addr := range slices.Sorted(maps.Keys(mk.services)) {
		s := mk.services[addr]
		fmt.Fprintf(tw, ":%s\n", addr)
		for _, fn := range slices.Sorted(maps.Keys(s.fns)) {
			h := s.fns[fn]
			sig := "(" + strings.Join(append([]string{fn}, h.params...), " ") + ")"
			if h.ask {
				sig = "ask " + sig
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", sig, mk.mark(s, fn), h.doc)
		}
		for _, topic := range slices.Sorted(maps.Keys(s.subs)) {
			sub := s.subs[topic]
			mk.seen = map[string]bool{}
			fmt.Fprintf(tw, "  on :%s (%s)\t%s\n", topic, sub.param, mk.forms(s, sub.body))
		}
		warnings = append(warnings, mk.missing(s)...)
	}
	tw.Flush()
	for _, w := range warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}
