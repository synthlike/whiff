package puff

import (
	"cmp"
	"errors"
	"fmt"
	"math"
)

// base is puff's own table: every Env starts with it.
var base = []*Builtin{
	{Name: "ok", Args: "v", Doc: "A successful Result.", Fn: tagFn("ok")},
	{Name: "err", Args: "e", Doc: "A failed Result.", Fn: tagFn("err")},
	{Name: "some", Args: "v", Doc: "An Option with a value.", Fn: tagFn("some")},
	{Name: "none", Doc: "An empty Option.", Fn: tagFn("none")},
	{Name: "fail", Args: "e", Doc: "Stop with a fault, e as its message.", Fn: fail},
	{Name: "help", Args: "[f]", Doc: "Print f's arguments, mark and doc, or list every builtin.", Fn: help},
	{Name: "=", Args: "a b", Doc: "Whether a and b are the same value.", Fn: func(_ *Call, args []Value) (Value, error) {
		return Bool(Equal(args[0], args[1])), nil
	}},
	{Name: "not", Args: "b", Doc: "The other bool.", Fn: not},
	{Name: "<", Args: "a b", Doc: "Whether a is less than b: ints, floats, strings or durations.", Fn: compareFn(func(c int) bool { return c < 0 })},
	{Name: "<=", Args: "a b", Doc: "Whether a is at most b.", Fn: compareFn(func(c int) bool { return c <= 0 })},
	{Name: ">", Args: "a b", Doc: "Whether a is greater than b.", Fn: compareFn(func(c int) bool { return c > 0 })},
	{Name: ">=", Args: "a b", Doc: "Whether a is at least b.", Fn: compareFn(func(c int) bool { return c >= 0 })},
	{Name: "+", Args: "a b ...", Doc: "The sum of ints, floats or durations.", Fn: add},
	{Name: "-", Args: "a [b]", Doc: "a minus b, or minus a.", Fn: sub},
	{Name: "*", Args: "a b ...", Doc: "The product of ints or floats.", Fn: mul},
}

func tagFn(name string) func(*Call, []Value) (Value, error) {
	return func(_ *Call, args []Value) (Value, error) { return NewTag(name, args...), nil }
}

func fail(_ *Call, args []Value) (Value, error) {
	if s, ok := args[0].(String); ok {
		return nil, errors.New(string(s))
	}
	return nil, errors.New(Print(args[0]))
}

func not(_ *Call, args []Value) (Value, error) {
	b, ok := args[0].(Bool)
	if !ok {
		return nil, fmt.Errorf("want a bool, got %s", Print(args[0]))
	}
	return !b, nil
}

func compareFn(ok func(int) bool) func(*Call, []Value) (Value, error) {
	return func(_ *Call, args []Value) (Value, error) {
		c, err := compare(args[0], args[1])
		if err != nil {
			return nil, err
		}
		return Bool(ok(c)), nil
	}
}

// compare orders two ints, floats, strings or durations of the same type.
func compare(a, b Value) (int, error) {
	switch a := a.(type) {
	case Int:
		if b, ok := b.(Int); ok {
			return cmp.Compare(a, b), nil
		}
	case Float:
		if b, ok := b.(Float); ok {
			return cmp.Compare(a, b), nil
		}
	case String:
		if b, ok := b.(String); ok {
			return cmp.Compare(a, b), nil
		}
	case Duration:
		if b, ok := b.(Duration); ok {
			return cmp.Compare(a, b), nil
		}
	}
	return 0, fmt.Errorf("can't compare %s and %s", typeName(a), typeName(b))
}

func add(_ *Call, args []Value) (Value, error) {
	acc := args[0]
	for _, v := range args[1:] {
		var err error
		if acc, err = arith("add", acc, v, addInt, func(x, y float64) float64 { return x + y }, true); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func sub(_ *Call, args []Value) (Value, error) {
	if len(args) == 2 {
		return arith("subtract", args[0], args[1], subInt, func(x, y float64) float64 { return x - y }, true)
	}
	switch a := args[0].(type) {
	case Int:
		return arith("negate", Int(0), a, subInt, nil, false)
	case Float:
		return -a, nil
	case Duration:
		return arith("negate", Duration(0), a, subInt, nil, true)
	}
	return nil, fmt.Errorf("can't negate %s", typeName(args[0]))
}

func mul(_ *Call, args []Value) (Value, error) {
	acc := args[0]
	for _, v := range args[1:] {
		var err error
		if acc, err = arith("multiply", acc, v, mulInt, func(x, y float64) float64 { return x * y }, false); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// arith applies an operation to two ints, floats or (if durations is set)
// durations of the same type. Ints and durations fault on overflow, floats
// on a result that isn't finite.
func arith(verb string, a, b Value, ints func(x, y int64) (int64, bool), floats func(x, y float64) float64, durations bool) (Value, error) {
	switch a := a.(type) {
	case Int:
		if b, ok := b.(Int); ok {
			c, ok := ints(int64(a), int64(b))
			if !ok {
				return nil, errors.New("int overflow")
			}
			return Int(c), nil
		}
	case Float:
		if b, ok := b.(Float); ok && floats != nil {
			c := floats(float64(a), float64(b))
			if math.IsInf(c, 0) || math.IsNaN(c) {
				return nil, errors.New("float overflow")
			}
			return Float(c), nil
		}
	case Duration:
		if b, ok := b.(Duration); ok && durations {
			c, ok := ints(int64(a), int64(b))
			if !ok {
				return nil, errors.New("duration overflow")
			}
			return Duration(c), nil
		}
	}
	return nil, fmt.Errorf("can't %s %s and %s", verb, typeName(a), typeName(b))
}

func addInt(a, b int64) (int64, bool) {
	c := a + b
	return c, !(b > 0 && c < a || b < 0 && c > a)
}

func subInt(a, b int64) (int64, bool) {
	c := a - b
	return c, !(b > 0 && c > a || b < 0 && c < a)
}

func mulInt(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	c := a * b
	return c, c/b == a && !(a == -1 && b == math.MinInt64) && !(b == -1 && a == math.MinInt64)
}

func typeName(v Value) string {
	switch v.(type) {
	case Bool:
		return "bool"
	case Int:
		return "int"
	case Float:
		return "float"
	case String:
		return "string"
	case Keyword:
		return "keyword"
	case Duration:
		return "duration"
	case Vector:
		return "vector"
	case Map:
		return "map"
	case Tag:
		return "tag"
	case *Func, *Builtin:
		return "function"
	}
	return fmt.Sprintf("%T", v)
}
