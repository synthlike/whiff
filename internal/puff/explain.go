package puff

import (
	"fmt"
	"slices"
	"strings"
)

// maxShown bounds a value or form printed in a fault, so a service's whole
// state doesn't flood the message.
const maxShown = 200

// short is s cut to maxShown characters.
func short(s string) string {
	if r := []rune(s); len(r) > maxShown {
		return string(r[:maxShown-1]) + "…"
	}
	return s
}

// mismatch is the fault message for v, which pattern p, of a let or a
// param, doesn't match: both, and the first place they differ.
func mismatch(what string, p *Form, v Value) string {
	msg := fmt.Sprintf("%s: %s doesn't match %s", what, short(Print(v)), short(p.String()))
	if why := explain(p, v, nil); why != "" {
		msg += ": " + why
	}
	return msg
}

// explain is why v doesn't match pattern p, "" if it does; path is where
// in the value matched it is, as keys and indexes.
func explain(p *Form, v Value, path []string) string {
	at := func(format string, args ...any) string {
		s := fmt.Sprintf(format, args...)
		if len(path) > 0 {
			s = "at " + strings.Join(path, " ") + ", " + s
		}
		return s
	}
	in := func(step string) []string { return append(slices.Clone(path), step) }
	switch p.Kind {
	case FormLiteral:
		if !Equal(v, p.Val) {
			return at("want %s, got %s", short(Print(p.Val)), short(Print(v)))
		}
	case FormList:
		name, args := p.Items[0].Name, p.Items[1:]
		t, ok := v.(Tag)
		if !ok || t.name != name || len(t.args) != len(args) {
			shape := "(" + name + strings.Repeat(" _", len(args)) + ")"
			return at("want %s, got %s", shape, short(Print(v)))
		}
		for i, a := range args {
			sub := path
			if len(args) > 1 {
				sub = in(fmt.Sprint(i))
			}
			if why := explain(a, t.args[i], sub); why != "" {
				return why
			}
		}
	case FormVector:
		items, rest := p.Items, (*Form)(nil)
		if i := slices.IndexFunc(items, func(it *Form) bool { return it.Kind == FormSymbol && it.Name == "&" }); i >= 0 {
			items, rest = items[:i], items[i+1]
		}
		vec, ok := v.(Vector)
		switch {
		case !ok:
			return at("want a vector, got %s", article(typeName(v)))
		case rest == nil && len(vec.items) != len(items):
			return at("want %s, got %d", count(len(items), "item"), len(vec.items))
		case len(vec.items) < len(items):
			return at("want at least %s, got %d", count(len(items), "item"), len(vec.items))
		}
		for i, it := range items {
			if why := explain(it, vec.items[i], in(fmt.Sprint(i))); why != "" {
				return why
			}
		}
		if rest != nil {
			return explain(rest, Vector{vec.items[len(items):]}, path)
		}
	case FormMap:
		m, ok := v.(Map)
		if !ok {
			return at("want a map, got %s", article(typeName(v)))
		}
		for i := 0; i+1 < len(p.Items); i += 2 {
			k := p.Items[i].Val
			x, ok := m.Get(k)
			if !ok {
				return at("no %s", Print(k))
			}
			if why := explain(p.Items[i+1], x, in(Print(k))); why != "" {
				return why
			}
		}
	}
	return ""
}

func article(s string) string {
	if s != "" && strings.ContainsRune("aeiou", rune(s[0])) {
		return "an " + s
	}
	return "a " + s
}

func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// local is a local name an expression refers to, and where its value is.
type local struct {
	name        string
	depth, slot int
}

// locals are the local names f refers to, once each, in order.
func locals(f *Form, s *scope) []local {
	var ls []local
	var walk func(f *Form)
	walk = func(f *Form) {
		if f.Kind == FormSymbol {
			if d, sl, ok := s.lookup(f.Name); ok && !slices.ContainsFunc(ls, func(l local) bool { return l.name == f.Name }) {
				ls = append(ls, local{f.Name, d, sl})
			}
			return
		}
		for _, it := range f.Items {
			walk(it)
		}
	}
	walk(f)
	return ls
}

// maxLocal bounds a local's printed value in an unwrap's fault: a short
// one, an id or a key, says which lookup it was; a long one is left out.
const maxLocal = 60

// unwrapFault is the fault of (unwrap arg) at pos, given v, which isn't
// (ok v) or (some v): arg's source, v, and the short locals arg names.
func unwrapFault(pos Pos, arg *Form, ls []local, fr *frame, v Value) *Fault {
	msg := fmt.Sprintf("unwrap: %s is %s", short(arg.String()), short(Print(v)))
	if arg.String() == Print(v) { // a literal: its source says nothing more
		msg = "unwrap: got " + short(Print(v))
	}
	if _, ok, err := unwrapped(v); err == nil && !ok {
		var where []string
		for _, l := range ls {
			if arg.Kind == FormSymbol {
				break // its value is v, already shown
			}
			f := fr
			for range l.depth {
				f = f.parent
			}
			if p := Print(f.vals[l.slot]); len([]rune(p)) <= maxLocal {
				where = append(where, l.name+" is "+p)
			}
		}
		if len(where) > 0 {
			msg += ", where " + strings.Join(where, ", ")
		}
	} else {
		msg += ", not a Result or an Option"
	}
	return &Fault{Pos: pos, Msg: msg}
}
