package puff

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// maxDepth bounds nested calls, so runaway recursion faults instead of
// exhausting Go's stack.
const maxDepth = 10000

// Fault stops an evaluation: a wrong call, a condition that isn't a bool,
// an overflow. Nothing in puff catches it; the host reports it.
type Fault struct {
	Pos Pos
	Msg string
}

func (f *Fault) Error() string { return f.Pos.String() + ": " + f.Msg }

func faultf(p Pos, format string, args ...any) *Fault {
	return &Fault{p, fmt.Sprintf(format, args...)}
}

// Func is a function defined in puff.
type Func struct {
	name    string // "" for an anonymous fn
	params  []string
	body    expr
	closure *frame
	refs    []*global // the globals its body refers to, for its mark
}

func (f *Func) print(b *strings.Builder) {
	b.WriteString("#<fn")
	if f.name != "" {
		b.WriteByte(' ')
		b.WriteString(f.name)
	}
	b.WriteByte('>')
}

// Env is a global scope: the builtins and the top-level defs of everything
// loaded into it. It is not safe for concurrent use.
type Env struct {
	// Stdout is where io/println and help write.
	Stdout io.Writer
	// Dir is where relative paths, os/cwd and proc/run start; "" is the
	// process's working directory.
	Dir     string
	globals map[string]*global
}

type global struct {
	val     Value // nil until its def has run
	builtin bool
}

// NewEnv returns an Env with puff's base table, writing to os.Stdout.
func NewEnv() *Env {
	e := &Env{Stdout: os.Stdout, globals: map[string]*global{}}
	if err := e.Register(base...); err != nil {
		panic(err)
	}
	return e
}

// Register adds builtins, such as a host's own on top of the base table.
// A name that is already taken is an error.
func (e *Env) Register(bs ...*Builtin) error {
	for _, b := range bs {
		if _, ok := e.globals[b.Name]; ok {
			return fmt.Errorf("builtin %s registered twice", b.Name)
		}
		own := *b
		own.min, own.max = parseArgs(b.Args)
		e.globals[b.Name] = &global{val: &own, builtin: true}
	}
	return nil
}

// Lookup returns the value of a global: a builtin, or a def that has run.
func (e *Env) Lookup(name string) (Value, bool) {
	g := e.globals[name]
	if g == nil || g.val == nil {
		return nil, false
	}
	return g.val, true
}

// Call calls the function f with args, as a host does: puff calling main,
// whiff calling a handler. pos is where a fault in the call itself points.
func (e *Env) Call(ctx context.Context, pos Pos, f Value, args ...Value) (Value, error) {
	t := &thread{ctx: ctx, env: e}
	return t.call(pos, f, args)
}

// Arity is the number of parameters f takes.
func (f *Func) Arity() int { return len(f.params) }

// Load reads src and evaluates its forms, as LoadForms does.
func (e *Env) Load(ctx context.Context, file, src string) (Value, error) {
	forms, err := Read(file, src)
	if err != nil {
		return nil, err
	}
	return e.LoadForms(ctx, forms)
}

// LoadForms checks and evaluates forms, and returns the value of the last:
// nil if that is a def or there are no forms. Names are checked before
// anything runs, so an unknown name fails the whole load with an *Error.
// Evaluation stops at a *Fault, keeping the defs that ran before it. A def
// of an existing name replaces it for everything that uses it.
func (e *Env) LoadForms(ctx context.Context, forms []*Form) (Value, error) {
	var err error
	c := &compiler{env: e, defined: map[string]*global{}, defPos: map[string]Pos{}}
	// Declare the defs first, so functions can call ones defined later.
	for _, f := range forms {
		if err := c.declare(f); err != nil {
			return nil, err
		}
	}
	steps := make([]expr, len(forms))
	for i, f := range forms {
		if isDef(f) {
			steps[i], err = c.def(f)
		} else {
			steps[i], err = c.expr(f, nil, false)
		}
		if err != nil {
			return nil, err
		}
	}
	maps.Copy(e.globals, c.defined)
	t := &thread{ctx: ctx, env: e}
	var v Value
	for i, step := range steps {
		if v, err = step(t, nil); err != nil {
			return nil, err
		}
		if i < len(steps)-1 && isErr(v) {
			return nil, faultf(forms[i].Pos, "dropped an err: %s", Print(v))
		}
	}
	return v, nil
}

// expr is compiled code: it evaluates in a thread with its local frame.
type expr func(t *thread, f *frame) (Value, error)

// thread is the state of one evaluation.
type thread struct {
	ctx   context.Context
	env   *Env
	depth int
}

// frame holds the values of local names. A frame is filled once, before
// anything can capture it, and never changes after.
type frame struct {
	vals   []Value
	parent *frame
}

type scopeKind int

const (
	scopeLet  scopeKind = iota // let bindings and match arms
	scopeFn                    // a function's params: recur doesn't reach past it
	scopeLoop                  // a loop's bindings, which recur rebinds
)

// scope mirrors a frame at compile time: the names of its slots.
type scope struct {
	kind   scopeKind
	names  []string
	parent *scope
}

func (s *scope) lookup(name string) (depth, slot int, ok bool) {
	for ; s != nil; s, depth = s.parent, depth+1 {
		for i := len(s.names) - 1; i >= 0; i-- {
			if s.names[i] == name {
				return depth, i, true
			}
		}
	}
	return 0, 0, false
}

// loop returns the scope of the loop a recur in s goes back to: the nearest
// one in the same function.
func (s *scope) loop() *scope {
	for ; s != nil && s.kind != scopeFn; s = s.parent {
		if s.kind == scopeLoop {
			return s
		}
	}
	return nil
}

var specialForms = map[string]bool{
	"def": true, "let": true, "fn": true, "if": true, "cond": true, "do": true, "and": true, "or": true,
	"match": true, "when": true, "loop": true, "recur": true, "->": true, "try": true, "catch": true,
}

// compiler turns forms into exprs, resolving every name.
type compiler struct {
	env     *Env
	defined map[string]*global // defs in this load, installed if it compiles
	defPos  map[string]Pos
	refs    []*global // globals referred to in the fn being compiled
}

func isDef(f *Form) bool {
	return f.Kind == FormList && len(f.Items) > 0 && f.Items[0].Kind == FormSymbol && f.Items[0].Name == "def"
}

// defName returns the name a (def name ...) or (def (name ...) ...) defines.
func defName(f *Form) (*Form, bool) {
	if !isDef(f) || len(f.Items) < 2 {
		return nil, false
	}
	switch t := f.Items[1]; t.Kind {
	case FormSymbol:
		return t, true
	case FormList:
		if len(t.Items) > 0 && t.Items[0].Kind == FormSymbol {
			return t.Items[0], true
		}
	}
	return nil, false
}

// declare notes the name of a top-level def.
func (c *compiler) declare(f *Form) error {
	name, ok := defName(f)
	if !ok {
		return nil
	}
	if pos, ok := c.defPos[name.Name]; ok {
		return errorf(name.Pos, "%s is already defined at %s", name.Name, pos)
	}
	if _, err := c.binding(name); err != nil {
		return err
	}
	g := c.env.globals[name.Name]
	if g == nil {
		g = &global{}
	} else if g.builtin {
		return errorf(name.Pos, "can't redefine builtin %s", name.Name)
	}
	c.defined[name.Name] = g
	c.defPos[name.Name] = name.Pos
	return nil
}

// def compiles (def name value) and (def (name params...) body...).
func (c *compiler) def(f *Form) (expr, error) {
	name, ok := defName(f)
	if !ok || len(f.Items) < 3 || f.Items[1].Kind == FormSymbol && len(f.Items) != 3 {
		return nil, errorf(f.Pos, "def wants a name and a value, or (name params...) and a body")
	}
	var val expr
	var err error
	if f.Items[1].Kind == FormSymbol {
		val, err = c.expr(f.Items[2], nil, false)
	} else {
		val, err = c.fn(name.Name, f.Pos, f.Items[1].Items[1:], f.Items[2:], nil)
	}
	if err != nil {
		return nil, err
	}
	g := c.defined[name.Name]
	return func(t *thread, fr *frame) (Value, error) {
		v, err := val(t, fr)
		if err != nil {
			return nil, err
		}
		g.val = v
		return nil, nil
	}, nil
}

// expr compiles f in scope s. tail is set when f's value is the value of
// the innermost loop body or function around it, the only place a recur
// can go.
func (c *compiler) expr(f *Form, s *scope, tail bool) (expr, error) {
	switch f.Kind {
	case FormLiteral:
		v := f.Val
		return func(*thread, *frame) (Value, error) { return v, nil }, nil
	case FormSymbol:
		return c.name(f, s)
	case FormVector:
		items, err := c.exprs(f.Items, s)
		if err != nil {
			return nil, err
		}
		return func(t *thread, fr *frame) (Value, error) {
			vs, err := evalAll(t, fr, items)
			if err != nil {
				return nil, err
			}
			return Vector{vs}, nil
		}, nil
	case FormMap:
		items, err := c.exprs(f.Items, s)
		if err != nil {
			return nil, err
		}
		return func(t *thread, fr *frame) (Value, error) {
			vs, err := evalAll(t, fr, items)
			if err != nil {
				return nil, err
			}
			es := make([]Entry, 0, len(vs)/2)
			for i := 0; i+1 < len(vs); i += 2 {
				es = append(es, Entry{vs[i], vs[i+1]})
			}
			m, err := NewMap(es...)
			if err != nil {
				return nil, faultf(f.Pos, "%s", err)
			}
			return m, nil
		}, nil
	}
	if len(f.Items) == 0 {
		return nil, errorf(f.Pos, "empty call ()")
	}
	if h := f.Items[0]; h.Kind == FormSymbol {
		switch h.Name {
		case "def":
			return nil, errorf(f.Pos, "def only at top level")
		case "let":
			return c.let(f, s, tail)
		case "fn":
			if len(f.Items) < 2 || f.Items[1].Kind != FormList {
				return nil, errorf(f.Pos, "fn wants (params...) and a body")
			}
			return c.fn("", f.Pos, f.Items[1].Items, f.Items[2:], s)
		case "if":
			return c.ifForm(f, s, tail)
		case "cond":
			return c.cond(f, s, tail)
		case "do":
			return c.body("do", f.Pos, f.Items[1:], s, tail)
		case "and":
			return c.logic(f, s, false)
		case "or":
			return c.logic(f, s, true)
		case "match":
			return c.match(f, s, tail)
		case "when":
			return nil, errorf(f.Pos, "when only goes in a match arm: [pattern when test body...]")
		case "loop":
			return c.loop(f, s)
		case "recur":
			return c.recur(f, s, tail)
		case "->":
			return c.thread(f, s, tail)
		case "try":
			return c.try(f, s)
		case "catch":
			return c.catch(f, s, tail)
		}
	}
	return c.call(f, s)
}

// exprs compiles forms that aren't in tail position.
func (c *compiler) exprs(fs []*Form, s *scope) ([]expr, error) {
	xs := make([]expr, len(fs))
	for i, f := range fs {
		x, err := c.expr(f, s, false)
		if err != nil {
			return nil, err
		}
		xs[i] = x
	}
	return xs, nil
}

func evalAll(t *thread, fr *frame, xs []expr) ([]Value, error) {
	vs := make([]Value, len(xs))
	for i, x := range xs {
		v, err := x(t, fr)
		if err != nil {
			return nil, err
		}
		vs[i] = v
	}
	return vs, nil
}

// name resolves a symbol to a local slot or a global.
func (c *compiler) name(f *Form, s *scope) (expr, error) {
	if specialForms[f.Name] {
		return nil, errorf(f.Pos, "%s is a special form, not a value", f.Name)
	}
	if depth, slot, ok := s.lookup(f.Name); ok {
		return func(_ *thread, fr *frame) (Value, error) {
			for range depth {
				fr = fr.parent
			}
			return fr.vals[slot], nil
		}, nil
	}
	g := c.defined[f.Name]
	if g == nil {
		g = c.env.globals[f.Name]
	}
	if g == nil {
		return nil, errorf(f.Pos, "unknown name %s", f.Name)
	}
	c.refs = append(c.refs, g)
	return func(*thread, *frame) (Value, error) {
		if g.val == nil {
			return nil, faultf(f.Pos, "%s used before its def", f.Name)
		}
		return g.val, nil
	}, nil
}

// binding checks a name about to be bound.
func (c *compiler) binding(f *Form) (string, error) {
	if f.Kind != FormSymbol {
		return "", errorf(f.Pos, "want a name, got %s", f)
	}
	if specialForms[f.Name] {
		return "", errorf(f.Pos, "can't bind special form %s", f.Name)
	}
	return f.Name, nil
}

// body compiles forms evaluated in order for the value of the last, which
// is in tail position if the body is. An (err e) from any other form
// faults: errors are handled or returned, not dropped.
func (c *compiler) body(what string, pos Pos, forms []*Form, s *scope, tail bool) (expr, error) {
	if len(forms) == 0 {
		return nil, errorf(pos, "%s needs a body", what)
	}
	xs, err := c.exprs(forms[:len(forms)-1], s)
	if err != nil {
		return nil, err
	}
	last, err := c.expr(forms[len(forms)-1], s, tail)
	if err != nil {
		return nil, err
	}
	if len(xs) == 0 {
		return last, nil
	}
	return func(t *thread, fr *frame) (Value, error) {
		for i, x := range xs {
			v, err := x(t, fr)
			if err != nil {
				return nil, err
			}
			if isErr(v) {
				return nil, faultf(forms[i].Pos, "dropped an err: %s", Print(v))
			}
		}
		return last(t, fr)
	}, nil
}

func isErr(v Value) bool {
	t, ok := v.(Tag)
	return ok && t.name == "err" && len(t.args) == 1
}

// bindings compiles a let or loop's [name value ...] into scope bs; each
// value sees the names bound before it.
func (c *compiler) bindings(f *Form, bs *scope) ([]expr, error) {
	if len(f.Items) < 2 || f.Items[1].Kind != FormVector || len(f.Items[1].Items)%2 != 0 {
		return nil, errorf(f.Pos, "%s wants [name value ...] bindings and a body", f.Items[0].Name)
	}
	items := f.Items[1].Items
	inits := make([]expr, 0, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		name, err := c.binding(items[i])
		if err != nil {
			return nil, err
		}
		x, err := c.expr(items[i+1], bs, false)
		if err != nil {
			return nil, err
		}
		inits = append(inits, x)
		bs.names = append(bs.names, name)
	}
	return inits, nil
}

// fill evaluates a let or loop's bindings into a new frame.
func fill(t *thread, fr *frame, inits []expr) (*frame, error) {
	lf := &frame{vals: make([]Value, len(inits)), parent: fr}
	for i, x := range inits {
		v, err := x(t, lf)
		if err != nil {
			return nil, err
		}
		lf.vals[i] = v
	}
	return lf, nil
}

// letBinding is one pattern and value of a let: the value's match fills
// the frame's slots from off on.
type letBinding struct {
	init  expr
	match matcher
	pos   Pos
	off   int
}

// let compiles (let [pattern value ...] body...). Each pattern is one of
// match's, so [a b] or {:k v} take a value apart; a value that doesn't
// match faults. A pattern's names are in scope from the next value on.
func (c *compiler) let(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) < 2 || f.Items[1].Kind != FormVector || len(f.Items[1].Items)%2 != 0 {
		return nil, errorf(f.Pos, "let wants [pattern value ...] bindings and a body")
	}
	ls := &scope{kind: scopeLet, parent: s}
	items := f.Items[1].Items
	bs := make([]letBinding, 0, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		x, err := c.expr(items[i+1], ls, false)
		if err != nil {
			return nil, err
		}
		// Its own names, so a later binding can shadow an earlier one.
		var names []string
		m, err := c.pattern(items[i], &names)
		if err != nil {
			return nil, err
		}
		bs = append(bs, letBinding{x, m, items[i].Pos, len(ls.names)})
		ls.names = append(ls.names, names...)
	}
	body, err := c.body("let", f.Pos, f.Items[2:], ls, tail)
	if err != nil {
		return nil, err
	}
	slots := len(ls.names)
	return func(t *thread, fr *frame) (Value, error) {
		lf := &frame{vals: make([]Value, slots), parent: fr}
		for _, b := range bs {
			v, err := b.init(t, lf)
			if err != nil {
				return nil, err
			}
			if !b.match(v, lf.vals[b.off:]) {
				return nil, faultf(b.pos, "let: no match for %s", Print(v))
			}
		}
		return body(t, lf)
	}, nil
}

// recurSignal is how a recur reaches its loop: it travels back as the
// error of the loop body, through tail positions only.
type recurSignal struct{ vals []Value }

func (*recurSignal) Error() string { return "recur outside a loop" }

// loop compiles (loop [name value ...] body...): the body runs again with
// new values each time it ends in (recur values...).
func (c *compiler) loop(f *Form, s *scope) (expr, error) {
	ls := &scope{kind: scopeLoop, parent: s}
	inits, err := c.bindings(f, ls)
	if err != nil {
		return nil, err
	}
	body, err := c.body("loop", f.Pos, f.Items[2:], ls, true)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		lf, err := fill(t, fr, inits)
		if err != nil {
			return nil, err
		}
		for {
			v, err := body(t, lf)
			r, ok := err.(*recurSignal)
			if !ok {
				return v, err
			}
			if t.ctx.Err() != nil {
				return nil, faultf(f.Pos, "cancelled")
			}
			lf = &frame{vals: r.vals, parent: fr}
		}
	}, nil
}

// recur compiles (recur values...), which must be in tail position of a
// loop in the same function and give a value for each of its bindings.
func (c *compiler) recur(f *Form, s *scope, tail bool) (expr, error) {
	l := s.loop()
	if l == nil {
		return nil, errorf(f.Pos, "recur outside a loop")
	}
	if !tail {
		return nil, errorf(f.Pos, "recur only in tail position")
	}
	if n := len(l.names); len(f.Items)-1 != n {
		return nil, errorf(f.Pos, "%s", arity("recur", n, n, len(f.Items)-1))
	}
	args, err := c.exprs(f.Items[1:], s)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		vs, err := evalAll(t, fr, args)
		if err != nil {
			return nil, err
		}
		return nil, &recurSignal{vs}
	}, nil
}

// thread compiles (-> value steps...) by rewriting it: each step is a call
// with the value so far as its first argument, and a bare name is a call
// with no other arguments. A (try f args...) step is (try (f value args...)).
func (c *compiler) thread(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) < 2 {
		return nil, errorf(f.Pos, "-> wants a value and steps")
	}
	acc := f.Items[1]
	for _, step := range f.Items[2:] {
		switch {
		case step.Kind == FormSymbol:
			acc = &Form{Kind: FormList, Pos: step.Pos, Items: []*Form{step, acc}}
		case step.Kind == FormList && len(step.Items) > 1 && step.Items[0].Kind == FormSymbol && step.Items[0].Name == "try":
			items := append([]*Form{step.Items[1], acc}, step.Items[2:]...)
			call := &Form{Kind: FormList, Pos: step.Pos, Items: items}
			acc = &Form{Kind: FormList, Pos: step.Pos, Items: []*Form{step.Items[0], call}}
		case step.Kind == FormList && len(step.Items) > 0:
			items := append([]*Form{step.Items[0], acc}, step.Items[1:]...)
			acc = &Form{Kind: FormList, Pos: step.Pos, Items: items}
		default:
			return nil, errorf(step.Pos, "-> steps are names or calls, got %s", step)
		}
	}
	return c.expr(acc, s, tail)
}

// returnSignal is how try returns an err from its function: it travels back
// as an error to the call.
type returnSignal struct{ val Value }

func (*returnSignal) Error() string { return "try outside a function" }

// try compiles (try result): (ok v) gives v, and (err e) returns from the
// function with the err, its position added to e's :trace.
func (c *compiler) try(f *Form, s *scope) (expr, error) {
	if len(f.Items) != 2 {
		return nil, errorf(f.Pos, "try takes one Result")
	}
	in := s
	for in != nil && in.kind != scopeFn {
		in = in.parent
	}
	if in == nil {
		return nil, errorf(f.Pos, "try only inside a function")
	}
	x, err := c.expr(f.Items[1], s, false)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		v, err := x(t, fr)
		if err != nil {
			return nil, err
		}
		if r, ok := v.(Tag); ok && len(r.args) == 1 {
			switch r.name {
			case "ok":
				return r.args[0], nil
			case "err":
				return nil, &returnSignal{NewTag("err", traced(r.args[0], f.Pos))}
			}
		}
		return nil, faultf(f.Pos, "try wants (ok v) or (err e), got %s", Print(v))
	}, nil
}

// traced adds pos to the :trace vector of an error map, so an error records
// the trys it went through. Other errors are left as they are.
func traced(e Value, pos Pos) Value {
	m, ok := e.(Map)
	if !ok {
		return e
	}
	var trace []Value
	if v, ok := m.Get(Keyword("trace")); ok {
		if vec, ok := v.(Vector); ok {
			trace = slices.Clone(vec.items)
		}
	}
	m, _ = m.With(Keyword("trace"), Vector{append(trace, String(pos.String()))})
	return m
}

// fn compiles a function's params and body; name is "" for an anonymous fn.
// A param is a name, or a vector or map pattern that takes its argument
// apart as let's do: (fn ([k v]) ...). An argument that doesn't match
// faults.
func (c *compiler) fn(name string, pos Pos, params, body []*Form, s *scope) (expr, error) {
	fs := &scope{kind: scopeFn, parent: s}
	// The patterns' names are in a scope of their own, inside the params'.
	ps := &scope{kind: scopeLet, parent: fs}
	type destructure struct {
		param int
		match matcher
		pos   Pos
	}
	var ds []destructure

	for i, p := range params {
		if p.Kind == FormVector || p.Kind == FormMap {
			m, err := c.pattern(p, &ps.names)
			if err != nil {
				return nil, err
			}
			ds = append(ds, destructure{i, m, p.Pos})
			fs.names = append(fs.names, p.String()) // no symbol can look it up
			continue
		}
		n, err := c.binding(p)
		if err != nil {
			return nil, err
		}
		fs.names = append(fs.names, n)
	}
	bound := slices.Clone(ps.names) // the patterns have checked their own
	for i, p := range params {
		if p.Kind != FormSymbol {
			continue
		}
		if slices.Contains(bound, fs.names[i]) {
			return nil, errorf(p.Pos, "parameter %s twice", fs.names[i])
		}
		bound = append(bound, fs.names[i])
	}
	what := name
	if what == "" {
		what = "fn"
	}
	outer := c.refs
	c.refs = nil
	bs := fs
	if len(ds) > 0 {
		bs = ps
	}
	b, err := c.body(what, pos, body, bs, true)
	if err != nil {
		return nil, err
	}
	if len(ds) > 0 {
		inner, slots := b, len(ps.names)
		b = func(t *thread, fr *frame) (Value, error) {
			pf := &frame{vals: make([]Value, slots), parent: fr}
			for _, d := range ds {
				if !d.match(fr.vals[d.param], pf.vals) {
					return nil, faultf(d.pos, "%s: no match for %s", what, Print(fr.vals[d.param]))
				}
			}
			return inner(t, pf)
		}
	}
	refs := c.refs
	c.refs = append(outer, refs...) // what an inner fn refers to, its outer one does too
	return func(_ *thread, fr *frame) (Value, error) {
		return &Func{name: name, params: fs.names, body: b, closure: fr, refs: refs}, nil
	}, nil
}

func (c *compiler) ifForm(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) != 4 {
		return nil, errorf(f.Pos, "if takes a condition and two branches")
	}
	cond, err := c.expr(f.Items[1], s, false)
	if err != nil {
		return nil, err
	}
	then, err := c.expr(f.Items[2], s, tail)
	if err != nil {
		return nil, err
	}
	els, err := c.expr(f.Items[3], s, tail)
	if err != nil {
		return nil, err
	}
	condPos := f.Items[1].Pos
	return func(t *thread, fr *frame) (Value, error) {
		ok, err := t.test(cond, fr, condPos)
		if err != nil {
			return nil, err
		}
		if ok {
			return then(t, fr)
		}
		return els(t, fr)
	}, nil
}

// cond compiles (cond [test body...] ... [_ body...]): the first arm whose
// test is #t runs, and a last arm of _ runs if none does. No arm matching
// faults.
func (c *compiler) cond(f *Form, s *scope, tail bool) (expr, error) {
	if len(f.Items) < 2 {
		return nil, errorf(f.Pos, "cond needs arms")
	}
	type arm struct {
		test expr
		pos  Pos
		body expr
	}
	var arms []arm
	for i, a := range f.Items[1:] {
		if a.Kind != FormVector || len(a.Items) < 2 {
			return nil, errorf(a.Pos, "cond arms are [test body...]")
		}
		var test expr
		if h := a.Items[0]; h.Kind == FormSymbol && h.Name == "_" {
			if i != len(f.Items)-2 {
				return nil, errorf(h.Pos, "cond's _ arm goes last")
			}
		} else {
			var err error
			if test, err = c.expr(h, s, false); err != nil {
				return nil, err
			}
		}
		body, err := c.body("cond arm", a.Pos, a.Items[1:], s, tail)
		if err != nil {
			return nil, err
		}
		arms = append(arms, arm{test, a.Items[0].Pos, body})
	}
	return func(t *thread, fr *frame) (Value, error) {
		for _, a := range arms {
			if a.test == nil {
				return a.body(t, fr)
			}
			ok, err := t.test(a.test, fr, a.pos)
			if err != nil {
				return nil, err
			}
			if ok {
				return a.body(t, fr)
			}
		}
		return nil, faultf(f.Pos, "no cond arm matched")
	}, nil
}

// logic compiles and (stop at the first #f) and or (stop at the first #t).
func (c *compiler) logic(f *Form, s *scope, stop bool) (expr, error) {
	if len(f.Items) < 2 {
		return nil, errorf(f.Pos, "%s needs conditions", f.Items[0].Name)
	}
	xs, err := c.exprs(f.Items[1:], s)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		for i, x := range xs {
			ok, err := t.test(x, fr, f.Items[i+1].Pos)
			if err != nil {
				return nil, err
			}
			if ok == stop {
				return Bool(stop), nil
			}
		}
		return Bool(!stop), nil
	}, nil
}

func (c *compiler) call(f *Form, s *scope) (expr, error) {
	xs, err := c.exprs(f.Items, s)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		vs, err := evalAll(t, fr, xs)
		if err != nil {
			return nil, err
		}
		return t.call(f.Pos, vs[0], vs[1:])
	}, nil
}

// test evaluates a condition, which must be a bool.
func (t *thread) test(x expr, fr *frame, pos Pos) (bool, error) {
	v, err := x(t, fr)
	if err != nil {
		return false, err
	}
	b, ok := v.(Bool)
	if !ok {
		return false, faultf(pos, "want a bool, got %s", Print(v))
	}
	return bool(b), nil
}

// call calls fn with args; pos is the call's, for faults.
func (t *thread) call(pos Pos, fn Value, args []Value) (Value, error) {
	if t.ctx.Err() != nil {
		return nil, faultf(pos, "cancelled")
	}
	switch fn := fn.(type) {
	case *Func:
		if len(args) != len(fn.params) {
			name := fn.name
			if name == "" {
				name = "fn"
			}
			return nil, faultf(pos, "%s", arity(name, len(fn.params), len(fn.params), len(args)))
		}
		if t.depth >= maxDepth {
			return nil, faultf(pos, "too deep: more than %d nested calls", maxDepth)
		}
		t.depth++
		defer func() { t.depth-- }()
		v, err := fn.body(t, &frame{vals: args, parent: fn.closure})
		if r, ok := err.(*returnSignal); ok {
			return r.val, nil
		}
		return v, err
	case *Builtin:
		if len(args) < fn.min || fn.max >= 0 && len(args) > fn.max {
			return nil, faultf(pos, "%s", arity(fn.Name, fn.min, fn.max, len(args)))
		}
		if what, ok := ReadOnly(t.ctx); ok && fn.Mark == MarkWrite && !fn.MarkByArgs {
			return nil, faultf(pos, "%s is :write, and %s is :read", fn.Name, what)
		}
		v, err := fn.Fn(&Call{Ctx: t.ctx, Env: t.env, t: t, pos: pos}, args)
		if err != nil {
			if _, ok := err.(*Fault); ok { // from a function it applied
				return nil, err
			}
			return nil, faultf(pos, "%s: %v", fn.Name, err)
		}
		return v, nil
	}
	return nil, faultf(pos, "not a function: %s", Print(fn))
}

func arity(name string, min, max, got int) string {
	switch {
	case min == max && min == 1:
		return fmt.Sprintf("%s takes 1 argument, got %d", name, got)
	case min == max:
		return fmt.Sprintf("%s takes %d arguments, got %d", name, min, got)
	case max < 0:
		return fmt.Sprintf("%s takes %d or more arguments, got %d", name, min, got)
	}
	return fmt.Sprintf("%s takes %d to %d arguments, got %d", name, min, max, got)
}
