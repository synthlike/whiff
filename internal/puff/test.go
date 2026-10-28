package puff

import (
	"context"
	"fmt"
)

// Test is a (test "name" body...) that a load found. Loading a file
// compiles its tests but doesn't run them; RunTest does. A test passes if
// its body's value is #t, or, for a body that ends in (= got want), if got
// equals want, which a failure then shows.
type Test struct {
	Name string
	Pos  Pos
	fn   expr // makes the body's function
	eq   bool // the body ends in (= got want), and gives [got want]
}

func isTest(f *Form) bool {
	return f.Kind == FormList && len(f.Items) > 0 && f.Items[0].Kind == FormSymbol && f.Items[0].Name == "test"
}

// test compiles (test "name" body...) as a function of no params, so try
// works in it.
func (c *compiler) test(f *Form) (*Test, error) {
	if len(f.Items) < 3 || f.Items[1].Kind != FormLiteral {
		return nil, errorf(f.Pos, "test wants a name and a body")
	}
	name, ok := f.Items[1].Val.(String)
	if !ok {
		return nil, errorf(f.Items[1].Pos, "test wants its name as a string, got %s", f.Items[1])
	}
	body := f.Items[2:]
	last := body[len(body)-1]
	eq := last.Kind == FormList && len(last.Items) == 3 && last.Items[0].Kind == FormSymbol && last.Items[0].Name == "="
	if eq {
		pair := &Form{Kind: FormVector, Pos: last.Pos, Items: last.Items[1:]}
		body = append(body[:len(body)-1:len(body)-1], pair)
	}
	fn, err := c.fn("test", f.Pos, nil, body, nil)
	if err != nil {
		return nil, err
	}
	return &Test{Name: string(name), Pos: f.Pos, fn: fn, eq: eq}, nil
}

// Tests are the tests of everything loaded into e, in order.
func (e *Env) Tests() []*Test { return e.tests }

// RunTest runs t in e: "" if it passed, else why it failed.
func (e *Env) RunTest(ctx context.Context, t *Test) string {
	th := &thread{ctx: ctx, env: e}
	fn, err := t.fn(th, nil)
	if err == nil {
		var v Value
		if v, err = th.call(t.Pos, fn, nil); err == nil {
			return verdict(t, v)
		}
	}
	return err.Error()
}

func verdict(t *Test, v Value) string {
	if t.eq {
		pair, ok := v.(Vector)
		if ok && len(pair.items) == 2 {
			if Equal(pair.items[0], pair.items[1]) {
				return ""
			}
			return fmt.Sprintf("got %s, want %s", Print(pair.items[0]), Print(pair.items[1]))
		}
		// An err returned by a try in the body.
		return fmt.Sprintf("got %s", Print(v))
	}
	if v == Bool(true) {
		return ""
	}
	return fmt.Sprintf("got %s, want #t", Print(v))
}
