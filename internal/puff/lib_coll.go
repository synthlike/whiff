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
	{Name: "vector/set", Args: "v i x", Doc: "v with the item at i, from 0, replaced by x.", Fn: vec1(func(v Vector, args []Value) (Value, error) {
		i, err := arg[Int](args, 1, "an int")
		if err != nil {
			return nil, err
		}
		if i < 0 || int(i) >= len(v.items) {
			return nil, fmt.Errorf("%d is out of range for length %d", i, len(v.items))
		}
		items := slices.Clone(v.items)
		items[i] = args[2]
		return Vector{items}, nil
	})},
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
	{Name: "vector/any?", Args: "v f", Doc: "Whether f is #t for some item.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		for _, x := range v.items {
			ok, err := applyTest(c, f, x)
			if err != nil || ok {
				return Bool(ok), err
			}
		}
		return Bool(false), nil
	})},
	{Name: "vector/all?", Args: "v f", Doc: "Whether f is #t for every item; #t for no items.", Fn: vecFn(func(c *Call, v Vector, f Value, _ []Value) (Value, error) {
		for _, x := range v.items {
			ok, err := applyTest(c, f, x)
			if err != nil || !ok {
				return Bool(ok), err
			}
		}
		return Bool(true), nil
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

// updateIn is map/set-in and map/update-in: args[0] with the value at
// args[1], a path, replaced by f of it. A missing key on the way is a new
// map if create, else a fault.
func updateIn(args []Value, create bool, f func(v Value) (Value, error)) (Value, error) {
	m, err := arg[Map](args, 0, "a map")
	if err != nil {
		return nil, err
	}
	path, err := arg[Vector](args, 1, "a path vector")
	if err != nil {
		return nil, err
	}
	if len(path.items) == 0 {
		return nil, fmt.Errorf("want a path of at least one key, got []")
	}
	var in func(m Map, keys []Value) (Value, error)
	in = func(m Map, keys []Value) (Value, error) {
		v, ok := m.Get(keys[0])
		switch {
		case !ok && !create:
			return nil, fmt.Errorf("no key %s", Print(keys[0]))
		case len(keys) == 1:
			v, err = f(v)
		default:
			next, isMap := v.(Map)
			if ok && !isMap {
				return nil, fmt.Errorf("want a map under %s, got %s", Print(keys[0]), Print(v))
			}
			v, err = in(next, keys[1:])
		}
		if err != nil {
			return nil, err
		}
		return m.With(keys[0], v)
	}
	return in(m, path.items)
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
	{Name: "map/update", Args: "m key f", Doc: "m with key's value v replaced by (f v); a key m doesn't have faults.", Fn: func(c *Call, args []Value) (Value, error) {
		m, err := arg[Map](args, 0, "a map")
		if err != nil {
			return nil, err
		}
		v, ok := m.Get(args[1])
		if !ok {
			return nil, fmt.Errorf("no key %s", Print(args[1]))
		}
		if v, err = c.Apply(args[2], v); err != nil {
			return nil, err
		}
		return m.With(args[1], v)
	}},
	{Name: "map/get-in", Args: "m path", Doc: "(some v): the value at path, a vector of keys, through nested maps; (none) if there isn't one.", Fn: map1(func(m Map, args []Value) (Value, error) {
		path, err := arg[Vector](args, 1, "a path vector")
		if err != nil {
			return nil, err
		}
		var v Value = m
		for _, k := range path.items {
			m, ok := v.(Map)
			if !ok {
				return None(), nil
			}
			if v, ok = m.Get(k); !ok {
				return None(), nil
			}
		}
		return Some(v), nil
	})},
	{Name: "map/set-in", Args: "m path v", Doc: "m with the value at path, a vector of keys, set to v, making the maps on the way that m doesn't have.", Fn: func(_ *Call, args []Value) (Value, error) {
		return updateIn(args, true, func(Value) (Value, error) { return args[2], nil })
	}},
	{Name: "map/update-in", Args: "m path f", Doc: "m with the value v at path, a vector of keys, replaced by (f v); a path m doesn't have faults.", Fn: func(c *Call, args []Value) (Value, error) {
		return updateIn(args, false, func(v Value) (Value, error) { return c.Apply(args[2], v) })
	}},
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
