package puff

import (
	"context"
	"fmt"
	"maps"
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
	params  int
	body    expr
	closure *frame
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
	globals map[string]*global
}

type global struct {
	val     Value // nil until its def has run
	builtin bool
}

// NewEnv returns an Env with the core builtins.
func NewEnv() *Env {
	e := &Env{globals: map[string]*global{}}
	for _, b := range core {
		e.globals[b.Name] = &global{val: b, builtin: true}
	}
	return e
}

// Load reads, checks and evaluates src, and returns the value of its last
// form: nil if that is a def or src has no forms. Names are checked before
// anything runs, so an unknown name fails the whole load with an *Error.
// Evaluation stops at a *Fault, keeping the defs that ran before it. A def
// of an existing name replaces it for everything that uses it.
func (e *Env) Load(ctx context.Context, file, src string) (Value, error) {
	forms, err := Read(file, src)
	if err != nil {
		return nil, err
	}
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
			steps[i], err = c.expr(f, nil)
		}
		if err != nil {
			return nil, err
		}
	}
	maps.Copy(e.globals, c.defined)
	t := &thread{ctx: ctx}
	var v Value
	for _, step := range steps {
		if v, err = step(t, nil); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// expr is compiled code: it evaluates in a thread with its local frame.
type expr func(t *thread, f *frame) (Value, error)

// thread is the state of one evaluation.
type thread struct {
	ctx   context.Context
	depth int
}

// frame holds the values of local names. A frame is filled once, before
// anything can capture it, and never changes after.
type frame struct {
	vals   []Value
	parent *frame
}

// scope mirrors a frame at compile time: the names of its slots.
type scope struct {
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

var specialForms = map[string]bool{
	"def": true, "let": true, "fn": true, "if": true, "cond": true, "do": true, "and": true, "or": true,
}

// compiler turns forms into exprs, resolving every name.
type compiler struct {
	env     *Env
	defined map[string]*global // defs in this load, installed if it compiles
	defPos  map[string]Pos
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
		val, err = c.expr(f.Items[2], nil)
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

func (c *compiler) expr(f *Form, s *scope) (expr, error) {
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
			return c.let(f, s)
		case "fn":
			if len(f.Items) < 2 || f.Items[1].Kind != FormList {
				return nil, errorf(f.Pos, "fn wants (params...) and a body")
			}
			return c.fn("", f.Pos, f.Items[1].Items, f.Items[2:], s)
		case "if":
			return c.ifForm(f, s)
		case "cond":
			return c.cond(f, s)
		case "do":
			return c.body("do", f.Pos, f.Items[1:], s)
		case "and":
			return c.logic(f, s, false)
		case "or":
			return c.logic(f, s, true)
		}
	}
	return c.call(f, s)
}

func (c *compiler) exprs(fs []*Form, s *scope) ([]expr, error) {
	xs := make([]expr, len(fs))
	for i, f := range fs {
		x, err := c.expr(f, s)
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

// body compiles forms evaluated in order for the value of the last.
func (c *compiler) body(what string, pos Pos, forms []*Form, s *scope) (expr, error) {
	if len(forms) == 0 {
		return nil, errorf(pos, "%s needs a body", what)
	}
	xs, err := c.exprs(forms, s)
	if err != nil {
		return nil, err
	}
	if len(xs) == 1 {
		return xs[0], nil
	}
	return func(t *thread, fr *frame) (Value, error) {
		for _, x := range xs[:len(xs)-1] {
			if _, err := x(t, fr); err != nil {
				return nil, err
			}
		}
		return xs[len(xs)-1](t, fr)
	}, nil
}

// let compiles (let [name value ...] body...); each value sees the names
// bound before it.
func (c *compiler) let(f *Form, s *scope) (expr, error) {
	if len(f.Items) < 2 || f.Items[1].Kind != FormVector || len(f.Items[1].Items)%2 != 0 {
		return nil, errorf(f.Pos, "let wants [name value ...] bindings and a body")
	}
	bs := f.Items[1].Items
	ls := &scope{parent: s}
	inits := make([]expr, 0, len(bs)/2)
	for i := 0; i < len(bs); i += 2 {
		name, err := c.binding(bs[i])
		if err != nil {
			return nil, err
		}
		x, err := c.expr(bs[i+1], ls)
		if err != nil {
			return nil, err
		}
		inits = append(inits, x)
		ls.names = append(ls.names, name)
	}
	body, err := c.body("let", f.Pos, f.Items[2:], ls)
	if err != nil {
		return nil, err
	}
	return func(t *thread, fr *frame) (Value, error) {
		lf := &frame{vals: make([]Value, len(inits)), parent: fr}
		for i, x := range inits {
			v, err := x(t, lf)
			if err != nil {
				return nil, err
			}
			lf.vals[i] = v
		}
		return body(t, lf)
	}, nil
}

// fn compiles a function's params and body; name is "" for an anonymous fn.
func (c *compiler) fn(name string, pos Pos, params, body []*Form, s *scope) (expr, error) {
	fs := &scope{parent: s}
	for _, p := range params {
		n, err := c.binding(p)
		if err != nil {
			return nil, err
		}
		if slices.Contains(fs.names, n) {
			return nil, errorf(p.Pos, "parameter %s twice", n)
		}
		fs.names = append(fs.names, n)
	}
	what := name
	if what == "" {
		what = "fn"
	}
	b, err := c.body(what, pos, body, fs)
	if err != nil {
		return nil, err
	}
	n := len(params)
	return func(_ *thread, fr *frame) (Value, error) {
		return &Func{name: name, params: n, body: b, closure: fr}, nil
	}, nil
}

func (c *compiler) ifForm(f *Form, s *scope) (expr, error) {
	if len(f.Items) != 4 {
		return nil, errorf(f.Pos, "if takes a condition and two branches")
	}
	xs, err := c.exprs(f.Items[1:], s)
	if err != nil {
		return nil, err
	}
	cond, then, els := xs[0], xs[1], xs[2]
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

// cond compiles (cond [test body...] ...): the first arm whose test is #t
// runs. No arm matching faults.
func (c *compiler) cond(f *Form, s *scope) (expr, error) {
	if len(f.Items) < 2 {
		return nil, errorf(f.Pos, "cond needs arms")
	}
	type arm struct {
		test expr
		pos  Pos
		body expr
	}
	var arms []arm
	for _, a := range f.Items[1:] {
		if a.Kind != FormVector || len(a.Items) < 2 {
			return nil, errorf(a.Pos, "cond arms are [test body...]")
		}
		test, err := c.expr(a.Items[0], s)
		if err != nil {
			return nil, err
		}
		body, err := c.body("cond arm", a.Pos, a.Items[1:], s)
		if err != nil {
			return nil, err
		}
		arms = append(arms, arm{test, a.Items[0].Pos, body})
	}
	return func(t *thread, fr *frame) (Value, error) {
		for _, a := range arms {
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
		if len(args) != fn.params {
			name := fn.name
			if name == "" {
				name = "fn"
			}
			return nil, faultf(pos, "%s", arity(name, fn.params, fn.params, len(args)))
		}
		if t.depth >= maxDepth {
			return nil, faultf(pos, "too deep: more than %d nested calls", maxDepth)
		}
		t.depth++
		defer func() { t.depth-- }()
		return fn.body(t, &frame{vals: args, parent: fn.closure})
	case *Builtin:
		if len(args) < fn.Min || fn.Max >= 0 && len(args) > fn.Max {
			return nil, faultf(pos, "%s", arity(fn.Name, fn.Min, fn.Max, len(args)))
		}
		v, err := fn.Fn(args)
		if err != nil {
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
