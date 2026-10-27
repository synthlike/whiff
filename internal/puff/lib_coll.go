package puff

import (
	"fmt"
	"slices"
)

// maxRange bounds vector/range, so a typo can't allocate the machine away.
const maxRange = 1 << 24

func vec1(f func(v Vector, args []Value) (Value, error)) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		v, err := arg[Vector](args, 0, "a vector")
		if err != nil {
			return nil, err
		}
		return f(v, args)
	}
}

// vecFn is vec1 for builtins that apply args[1], a function, to items.
func vecFn(f func(c *Call, v Vector, fn Value, args []Value) (Value, error)) builtinFn {
	return func(c *Call, args []Value) (Value, error) {
		v, err := arg[Vector](args, 0, "a vector")
		if err != nil {
			return nil, err
		}
		return f(c, v, args[len(args)-1], args)
	}
}

var vectorFns = []*Builtin{
	{Name: "vector/length", Args: "v", Doc: "The number of items in v.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		return Int(len(v.items)), nil
	})},
	{Name: "vector/empty?", Args: "v", Doc: "Whether v has no items.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		return Bool(len(v.items) == 0), nil
	})},
	{Name: "vector/get", Args: "v i", Doc: "(some x): the item at i, from 0; (none) if there isn't one.", Fn: vec1(func(v Vector, args []Value) (Value, error) {
		i, err := arg[Int](args, 1, "an int")
		if err != nil {
			return nil, err
		}
		if i < 0 || i >= Int(len(v.items)) {
			return None(), nil
		}
		return Some(v.items[i]), nil
	})},
	{Name: "vector/first", Args: "v", Doc: "(some x): the first item; (none) if v is empty.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		if len(v.items) == 0 {
			return None(), nil
		}
		return Some(v.items[0]), nil
	})},
	{Name: "vector/last", Args: "v", Doc: "(some x): the last item; (none) if v is empty.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		if len(v.items) == 0 {
			return None(), nil
		}
		return Some(v.items[len(v.items)-1]), nil
	})},
	{Name: "vector/append", Args: "v x ...", Doc: "v with the xs added at the end.", Fn: vec1(func(v Vector, args []Value) (Value, error) {
		return Vector{slices.Concat(v.items, args[1:])}, nil
	})},
	{Name: "vector/concat", Args: "v ...", Doc: "The items of every v, in order.", Fn: func(_ *Call, args []Value) (Value, error) {
		var items []Value
		for i := range args {
			v, err := arg[Vector](args, i, "a vector")
			if err != nil {
				return nil, err
			}
			items = append(items, v.items...)
		}
		return Vector{items}, nil
	}},
	{Name: "vector/slice", Args: "v start [end]", Doc: "The items from start up to end, or to the end of v.", Fn: vec1(func(v Vector, args []Value) (Value, error) {
		start, end, err := span(args, 1, len(v.items))
		if err != nil {
			return nil, err
		}
		return Vector{v.items[start:end]}, nil
	})},
	{Name: "vector/reverse", Args: "v", Doc: "v's items in reverse order.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		items := slices.Clone(v.items)
		slices.Reverse(items)
		return Vector{items}, nil
	})},
	{Name: "vector/contains?", Args: "v x", Doc: "Whether an item of v equals x.", Fn: vec1(func(v Vector, args []Value) (Value, error) {
		return Bool(slices.ContainsFunc(v.items, func(it Value) bool { return Equal(it, args[1]) })), nil
	})},
	{Name: "vector/range", Args: "start end", Doc: "The ints from start up to end.", Fn: func(_ *Call, args []Value) (Value, error) {
		start, err := arg[Int](args, 0, "an int")
		if err != nil {
			return nil, err
		}
		end, err := arg[Int](args, 1, "an int")
		if err != nil {
			return nil, err
		}
		if end > start && uint64(end-start) > maxRange {
			return nil, fmt.Errorf("more than %d ints", maxRange)
		}
		var items []Value
		for i := start; i < end; i++ {
			items = append(items, i)
		}
		return Vector{items}, nil
	}},
	{Name: "vector/sort", Args: "v", Doc: "v's items in ascending order: ints, floats, strings or durations.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		items := slices.Clone(v.items)
		var cerr error
		slices.SortStableFunc(items, func(a, b Value) int {
			c, err := compare(a, b)
			if err != nil && cerr == nil {
				cerr = err
			}
			return c
		})
		if cerr != nil {
			return nil, cerr
		}
		return Vector{items}, nil
	})},
	{Name: "vector/map", Args: "v f", Doc: "f of each item.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		items := make([]Value, len(v.items))
		for i, x := range v.items {
			r, err := c.Apply(f, x)
			if err != nil {
				return nil, err
			}
			items[i] = r
		}
		return Vector{items}, nil
	})},
	{Name: "vector/filter", Args: "v f", Doc: "The items for which f is #t.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		var items []Value
		for _, x := range v.items {
			ok, err := applyTest(c, f, x)
			if err != nil {
				return nil, err
			}
			if ok {
				items = append(items, x)
			}
		}
		return Vector{items}, nil
	})},
	{Name: "vector/find", Args: "v f", Doc: "(some x): the first item for which f is #t; (none) if there isn't one.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		for _, x := range v.items {
			ok, err := applyTest(c, f, x)
			if err != nil {
				return nil, err
			}
			if ok {
				return Some(x), nil
			}
		}
		return None(), nil
	})},
	{Name: "vector/reduce", Args: "v init f", Doc: "(f (f init x1) x2) ... over the items.", Fn: vecFn(func(c *Call, v Vector, f Value, args []Value) (Value, error) {
		acc := args[1]
		for _, x := range v.items {
			var err error
			if acc, err = c.Apply(f, acc, x); err != nil {
				return nil, err
			}
		}
		return acc, nil
	})},
	{Name: "vector/each", Args: "v f", Doc: "Call f on each item for its effect: (ok :done), or the first (err e) f returns.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		for _, x := range v.items {
			r, err := c.Apply(f, x)
			if err != nil {
				return nil, err
			}
			if isErr(r) {
				return r, nil
			}
		}
		return Done, nil
	})},
}

// applyTest applies f, which must return a bool.
func applyTest(c *Call, f, x Value) (bool, error) {
	r, err := c.Apply(f, x)
	if err != nil {
		return false, err
	}
	b, ok := r.(Bool)
	if !ok {
		return false, fmt.Errorf("want a bool, got %s", Print(r))
	}
	return bool(b), nil
}

func map1(f func(m Map, args []Value) (Value, error)) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		m, err := arg[Map](args, 0, "a map")
		if err != nil {
			return nil, err
		}
		return f(m, args)
	}
}

var mapFns = []*Builtin{
	{Name: "map/get", Args: "m key", Doc: "(some v): the value under key; (none) if there isn't one.", Fn: map1(func(m Map, args []Value) (Value, error) {
		if v, ok := m.Get(args[1]); ok {
			return Some(v), nil
		}
		return None(), nil
	})},
	{Name: "map/get-or", Args: "m key default", Doc: "The value under key, or default.", Fn: map1(func(m Map, args []Value) (Value, error) {
		if v, ok := m.Get(args[1]); ok {
			return v, nil
		}
		return args[2], nil
	})},
	{Name: "map/has?", Args: "m key", Doc: "Whether m has key.", Fn: map1(func(m Map, args []Value) (Value, error) {
		_, ok := m.Get(args[1])
		return Bool(ok), nil
	})},
	{Name: "map/set", Args: "m key v", Doc: "m with key set to v.", Fn: map1(func(m Map, args []Value) (Value, error) {
		return m.With(args[1], args[2])
	})},
	{Name: "map/delete", Args: "m key", Doc: "m without key.", Fn: map1(func(m Map, args []Value) (Value, error) {
		if _, ok := m.Get(args[1]); !ok {
			return m, nil
		}
		i, _ := m.find(args[1])
		return Map{slices.Delete(slices.Clone(m.entries), i, i+1)}, nil
	})},
	{Name: "map/merge", Args: "m ...", Doc: "The maps' entries together; a later map's value wins.", Fn: func(_ *Call, args []Value) (Value, error) {
		var out Map
		for i := range args {
			m, err := arg[Map](args, i, "a map")
			if err != nil {
				return nil, err
			}
			for _, e := range m.entries {
				out, _ = out.With(e.Key, e.Val)
			}
		}
		return out, nil
	}},
	{Name: "map/keys", Args: "m", Doc: "m's keys, in print order.", Fn: map1(func(m Map, _ []Value) (Value, error) {
		items := make([]Value, len(m.entries))
		for i, e := range m.entries {
			items[i] = e.Key
		}
		return Vector{items}, nil
	})},
	{Name: "map/values", Args: "m", Doc: "m's values, in the order of its keys.", Fn: map1(func(m Map, _ []Value) (Value, error) {
		items := make([]Value, len(m.entries))
		for i, e := range m.entries {
			items[i] = e.Val
		}
		return Vector{items}, nil
	})},
	{Name: "map/entries", Args: "m", Doc: "m's [key value] pairs, in print order.", Fn: map1(func(m Map, _ []Value) (Value, error) {
		items := make([]Value, len(m.entries))
		for i, e := range m.entries {
			items[i] = Vector{[]Value{e.Key, e.Val}}
		}
		return Vector{items}, nil
	})},
	{Name: "map/from-entries", Args: "pairs", Doc: "A map of [key value] pairs; a later pair's value wins.", Fn: vec1(func(v Vector, _ []Value) (Value, error) {
		var out Map
		for _, it := range v.items {
			pair, ok := it.(Vector)
			if !ok || len(pair.items) != 2 {
				return nil, fmt.Errorf("want a [key value] pair, got %s", Print(it))
			}
			var err error
			if out, err = out.With(pair.items[0], pair.items[1]); err != nil {
				return nil, err
			}
		}
		return out, nil
	})},
	{Name: "map/length", Args: "m", Doc: "The number of entries in m.", Fn: map1(func(m Map, _ []Value) (Value, error) {
		return Int(len(m.entries)), nil
	})},
	{Name: "map/empty?", Args: "m", Doc: "Whether m has no entries.", Fn: map1(func(m Map, _ []Value) (Value, error) {
		return Bool(len(m.entries) == 0), nil
	})},
}
