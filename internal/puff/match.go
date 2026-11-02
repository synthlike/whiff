package puff

import "slices"

// matcher tests a value against a pattern, filling the slots of the names
// the pattern binds.
type matcher func(v Value, slots []Value) bool

// match compiles (match value [pattern body...] ...). An arm can have a
// guard: [pattern when test body...]. The first arm that matches runs; no
// arm matching faults.
func (c *compiler) match(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) < 3 {
		return nil, errorf(f.Pos, "match wants a value and [pattern body...] arms")
	}
	subject, err := c.expr(f.Items[1], s, false)
	if err != nil {
		return nil, err
	}
	arms, err := c.arms("match", f.Items[2:], s, tail)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		v, err := subject(t, fr)
		if err != nil {
			return nil, err
		}
		r, ok, err := t.arms(arms, v, fr)
		if err == nil && !ok {
			err = faultf(f.Pos, "no match for %s", short(Print(v)))
		}
		return r, err
	}, nil
}

// catch compiles (catch value [pattern body...] ...), whose arms are
// match's: an (err e) from value has e matched against them, and the first
// arm that matches gives catch's value. An err no arm matches, or anything
// but an err, is catch's value as it is: catch handles the errs it knows,
// and the rest go on.
func (c *compiler) catch(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) < 3 {
		return nil, errorf(f.Pos, "catch wants a value and [pattern body...] arms")
	}
	subject, err := c.expr(f.Items[1], s, false)
	if err != nil {
		return nil, err
	}
	arms, err := c.arms("catch", f.Items[2:], s, tail)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		v, err := subject(t, fr)
		if err != nil || !isErr(v) {
			return v, err
		}
		r, ok, err := t.arms(arms, v.(Tag).args[0], fr)
		if err == nil && !ok {
			return v, nil
		}
		return r, err
	}, nil
}

type arm struct {
	match    matcher
	slots    int
	guard    expr // nil without when
	guardPos Pos
	body     expr
}

// arms compiles the [pattern body...] arms of a match or catch.
func (c *compiler) arms(what string, fs []*Form, s *scope, tail bool) ([]arm, error) {
	var arms []arm
	for _, a := range fs {
		if a.Kind != FormVector || len(a.Items) < 2 {
			return nil, errorf(a.Pos, "%s arms are [pattern body...]", what)
		}
		as := &scope{kind: scopeLet, parent: s}
		m, err := c.pattern(a.Items[0], &as.names)
		if err != nil {
			return nil, err
		}
		rest := a.Items[1:]
		var guard expr
		var guardPos Pos
		if h := rest[0]; h.Kind == FormSymbol && h.Name == "when" {
			if len(rest) < 3 {
				return nil, errorf(a.Pos, "%s arms with a guard are [pattern when test body...]", what)
			}
			if guard, err = c.expr(rest[1], as, false); err != nil {
				return nil, err
			}
			guardPos = rest[1].Pos
			rest = rest[2:]
		}
		body, err := c.body(what+" arm", a.Pos, rest, as, tail)
		if err != nil {
			return nil, err
		}
		arms = append(arms, arm{m, len(as.names), guard, guardPos, body})
	}
	return arms, nil
}

// arms runs the first of arms that matches v: its value, and whether one
// did.
func (t *thread) arms(arms []arm, v Value, fr *frame) (Value, bool, error) {
	for _, a := range arms {
		af := &frame{vals: make([]Value, a.slots), parent: fr}
		if !a.match(v, af.vals) {
			continue
		}
		if a.guard != nil {
			ok, err := t.test(a.guard, af, a.guardPos)
			if err != nil {
				return nil, true, err
			}
			if !ok {
				continue
			}
		}
		r, err := a.body(t, af)
		return r, true, err
	}
	return nil, false, nil
}

// pattern compiles a pattern, appending the names it binds to names:
// literals match equal values, _ anything, a name anything and binds it,
// (name patterns...) a tag, [patterns... & rest] a vector, and {key pattern
// ...} a map with at least those keys.
func (c *compiler) pattern(f *Form, names *[]string) (matcher, error) {
	switch f.Kind {
	case FormLiteral:
		lit := f.Val
		return func(v Value, _ []Value) bool { return Equal(v, lit) }, nil
	case FormSymbol:
		switch f.Name {
		case "_":
			return func(Value, []Value) bool { return true }, nil
		case "&":
			return nil, errorf(f.Pos, "& only goes in vector patterns, before the rest")
		}
		name, err := c.binding(f)
		if err != nil {
			return nil, err
		}
		if slices.Contains(*names, name) {
			return nil, errorf(f.Pos, "%s bound twice in pattern", name)
		}
		slot := len(*names)
		*names = append(*names, name)
		return func(v Value, slots []Value) bool {
			slots[slot] = v
			return true
		}, nil
	case FormList:
		if len(f.Items) == 0 || f.Items[0].Kind != FormSymbol {
			return nil, errorf(f.Pos, "tag patterns are (name patterns...)")
		}
		name := f.Items[0].Name
		args, err := c.patterns(f.Items[1:], names)
		if err != nil {
			return nil, err
		}
		return func(v Value, slots []Value) bool {
			t, ok := v.(Tag)
			if !ok || t.name != name || len(t.args) != len(args) {
				return false
			}
			for i, m := range args {
				if !m(t.args[i], slots) {
					return false
				}
			}
			return true
		}, nil
	case FormVector:
		items := f.Items
		var restForm *Form
		if i := slices.IndexFunc(items, func(it *Form) bool { return it.Kind == FormSymbol && it.Name == "&" }); i >= 0 {
			if i != len(items)-2 {
				return nil, errorf(items[i].Pos, "& takes one pattern, at the end")
			}
			restForm, items = items[i+1], items[:i]
		}
		ms, err := c.patterns(items, names)
		if err != nil {
			return nil, err
		}
		var rest matcher
		if restForm != nil {
			if rest, err = c.pattern(restForm, names); err != nil {
				return nil, err
			}
		}
		return func(v Value, slots []Value) bool {
			vec, ok := v.(Vector)
			if !ok || len(vec.items) < len(ms) || rest == nil && len(vec.items) != len(ms) {
				return false
			}
			for i, m := range ms {
				if !m(vec.items[i], slots) {
					return false
				}
			}
			return rest == nil || rest(Vector{vec.items[len(ms):]}, slots)
		}, nil
	}
	// A map; the reader has checked that literal keys can be keys.
	type entry struct {
		key Value
		m   matcher
	}
	var es []entry
	for i := 0; i < len(f.Items); i += 2 {
		k := f.Items[i]
		if k.Kind != FormLiteral {
			return nil, errorf(k.Pos, "map pattern keys must be literals")
		}
		m, err := c.pattern(f.Items[i+1], names)
		if err != nil {
			return nil, err
		}
		es = append(es, entry{k.Val, m})
	}
	return func(v Value, slots []Value) bool {
		mv, ok := v.(Map)
		if !ok {
			return false
		}
		for _, e := range es {
			x, ok := mv.Get(e.key)
			if !ok || !e.m(x, slots) {
				return false
			}
		}
		return true
	}, nil
}

func (c *compiler) patterns(fs []*Form, names *[]string) ([]matcher, error) {
	ms := make([]matcher, len(fs))
	for i, f := range fs {
		m, err := c.pattern(f, names)
		if err != nil {
			return nil, err
		}
		ms[i] = m
	}
	return ms, nil
}
