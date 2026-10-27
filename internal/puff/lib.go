package puff

import (
	"fmt"
	"slices"
)

// base is puff's own table: every Env starts with it.
var base = slices.Concat(coreFns, stringFns, numberFns, valueFns, vectorFns, mapFns, regexFns, jsonFns)

type builtinFn = func(c *Call, args []Value) (Value, error)

// arg returns args[i] as a T, or faults; what names T for the message,
// "a string".
func arg[T Value](args []Value, i int, what string) (T, error) {
	v, ok := args[i].(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("want %s, got %s", what, Print(args[i]))
	}
	return v, nil
}

// options returns the options map at args[i], empty if it isn't given. A
// key that isn't one of names faults, so a typo doesn't go unnoticed.
func options(args []Value, i int, names ...string) (Map, error) {
	if i >= len(args) {
		return Map{}, nil
	}
	m, err := arg[Map](args, i, "an options map")
	if err != nil {
		return Map{}, err
	}
	for _, e := range m.entries {
		if k, ok := e.Key.(Keyword); !ok || !slices.Contains(names, string(k)) {
			return Map{}, fmt.Errorf("unknown option %s", Print(e.Key))
		}
	}
	return m, nil
}

// span reads a start, and an optional end, from args[i:] as a range of a
// sequence of length n.
func span(args []Value, i, n int) (start, end int, err error) {
	s, err := arg[Int](args, i, "an int")
	if err != nil {
		return 0, 0, err
	}
	e := Int(n)
	if len(args) > i+1 {
		if e, err = arg[Int](args, i+1, "an int"); err != nil {
			return 0, 0, err
		}
	}
	if s < 0 || s > e || e > Int(n) {
		return 0, 0, fmt.Errorf("%d to %d is out of range for length %d", s, e, n)
	}
	return int(s), int(e), nil
}

func strVec(ss []string) Vector {
	vs := make([]Value, len(ss))
	for i, s := range ss {
		vs[i] = String(s)
	}
	return Vector{vs}
}
