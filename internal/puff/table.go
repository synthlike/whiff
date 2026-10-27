package puff

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Builtin is a function implemented in Go: one entry of a table.
type Builtin struct {
	Name string
	// Args names the arguments for help and sets the arity: required
	// names, then optional ones in brackets, then ... for any number more
	// of the last: "s sep", "a [b]", "a b ...".
	Args string
	Mark Mark
	Doc  string
	// Fn returns the call's value. A failure for the caller to handle is a
	// value, (err e) from Errorf; a returned error is a fault: a wrong
	// argument, an overflow.
	Fn func(c *Call, args []Value) (Value, error)

	min, max int // from Args; max is -1 for any number
}

func (f *Builtin) print(b *strings.Builder) {
	b.WriteString("#<builtin ")
	b.WriteString(f.Name)
	b.WriteByte('>')
}

// Call is what a builtin gets besides its arguments.
type Call struct {
	Ctx context.Context
	Env *Env
}

// parseArgs returns the arity Args describes.
func parseArgs(args string) (min, max int) {
	opt, more := 0, false
	for _, a := range strings.Fields(args) {
		switch {
		case a == "...":
			more = true
		case strings.HasPrefix(a, "["):
			opt++
		default:
			min++
		}
	}
	if more {
		return min, -1
	}
	return min, min + opt
}

// Ok, Some and None build Results and Options.
func Ok(v Value) Value   { return NewTag("ok", v) }
func Some(v Value) Value { return NewTag("some", v) }
func None() Value        { return NewTag("none") }

// Done is the value of an effect with nothing else to return.
var Done = Ok(Keyword("done"))

// Errorf returns (err {:kind kind :message msg}): how a builtin reports a
// failure for its caller to handle.
func Errorf(kind, format string, args ...any) Value {
	m, _ := NewMap(
		Entry{Keyword("kind"), Keyword(kind)},
		Entry{Keyword("message"), String(fmt.Sprintf(format, args...))},
	)
	return NewTag("err", m)
}

// Mark says whether a function only reads, or changes something that is
// yours: a file, a remote, a service's state.
type Mark int

const (
	MarkRead Mark = iota
	MarkWrite
)

func (m Mark) String() string {
	if m == MarkWrite {
		return ":write"
	}
	return ":read"
}

// MarkOf returns v's mark. A builtin's is in its table entry. A puff
// function is :write if anything it refers to is: the globals in its body,
// followed through other functions, and the values it closed over. Data is
// :write if it holds a :write function. Marks are worked out each time, so
// redefining a helper changes the marks of its users.
func MarkOf(v Value) Mark {
	seen := map[*Func]bool{}
	var walk func(v Value) bool
	walk = func(v Value) bool {
		switch v := v.(type) {
		case *Builtin:
			return v.Mark == MarkWrite
		case *Func:
			if seen[v] {
				return false
			}
			seen[v] = true
			for _, g := range v.refs {
				if g.val != nil && walk(g.val) {
					return true
				}
			}
			for fr := v.closure; fr != nil; fr = fr.parent {
				if slices.ContainsFunc(fr.vals, walk) {
					return true
				}
			}
		case Vector:
			return slices.ContainsFunc(v.items, walk)
		case Tag:
			return slices.ContainsFunc(v.args, walk)
		case Map:
			return slices.ContainsFunc(v.entries, func(e Entry) bool { return walk(e.Val) })
		}
		return false
	}
	if walk(v) {
		return MarkWrite
	}
	return MarkRead
}

// help prints a function's arguments, mark and doc, or every builtin's
// arguments and mark.
func help(c *Call, args []Value) (Value, error) {
	w := c.Env.Stdout
	if len(args) == 0 {
		for _, name := range slices.Sorted(maps.Keys(c.Env.globals)) {
			if g := c.Env.globals[name]; g.builtin {
				b := g.val.(*Builtin)
				fmt.Fprintf(w, "%s %s\n", signature(b.Name, b.Args), b.Mark)
			}
		}
		return Done, nil
	}
	switch f := args[0].(type) {
	case *Builtin:
		fmt.Fprintf(w, "%s %s\n  %s\n", signature(f.Name, f.Args), f.Mark, f.Doc)
	case *Func:
		name := f.name
		if name == "" {
			name = "fn"
		}
		fmt.Fprintf(w, "%s %s\n", signature(name, strings.Join(f.params, " ")), MarkOf(f))
	default:
		return nil, fmt.Errorf("want a function, got %s", Print(args[0]))
	}
	return Done, nil
}

func signature(name, args string) string {
	if args == "" {
		return "(" + name + ")"
	}
	return "(" + name + " " + args + ")"
}
