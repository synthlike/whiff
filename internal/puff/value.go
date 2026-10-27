// Package puff is a small lisp for whiff's services: immutable values, and
// Results and Options instead of nil and exceptions.
package puff

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Value is a puff value. Values are immutable and compare with Equal.
type Value interface {
	// print appends v's readable form to b.
	print(b *strings.Builder)
}

type (
	Bool     bool
	Int      int64
	Float    float64
	String   string
	Keyword  string // the name, without the leading colon
	Duration time.Duration
)

// Time is a point in time, as durations are spans: 2026-10-27T10:00:00Z. It
// is kept as UTC nanoseconds since 1970, so equal times are equal values;
// that covers the years 1678 to 2262.
type Time struct{ ns int64 }

// NewTime returns t as a puff time; t must be within timeFits's range.
func NewTime(t time.Time) Time { return Time{t.UnixNano()} }

// Go returns v as a Go time, in UTC.
func (v Time) Go() time.Time { return time.Unix(0, v.ns).UTC() }

// timeFits reports whether t is within the range a Time holds.
func timeFits(t time.Time) bool { return time.Unix(0, t.UnixNano()).Equal(t) }

// Vector is an immutable sequence: [1 2 3].
type Vector struct{ items []Value }

// NewVector returns a vector of a copy of items.
func NewVector(items ...Value) Vector { return Vector{slices.Clone(items)} }

func (v Vector) Len() int       { return len(v.items) }
func (v Vector) At(i int) Value { return v.items[i] }

// Map is an immutable map with bool, int, string or keyword keys, kept in
// print order: {:a 1 :b 2}.
type Map struct{ entries []Entry }

type Entry struct{ Key, Val Value }

// NewMap returns a map of entries, in any order. It fails on a key that
// isn't a bool, int, string or keyword, and on a duplicate key.
func NewMap(entries ...Entry) (Map, error) {
	es := slices.Clone(entries)
	for _, e := range es {
		if keyRank(e.Key) < 0 {
			return Map{}, fmt.Errorf("map key %s: must be a bool, int, string or keyword", Print(e.Key))
		}
	}
	slices.SortFunc(es, func(a, b Entry) int { return compareKeys(a.Key, b.Key) })
	for i := 1; i < len(es); i++ {
		if compareKeys(es[i-1].Key, es[i].Key) == 0 {
			return Map{}, fmt.Errorf("duplicate map key %s", Print(es[i].Key))
		}
	}
	return Map{es}, nil
}

func (m Map) Len() int { return len(m.entries) }

// Entry returns the i-th entry in print order.
func (m Map) Entry(i int) Entry { return m.entries[i] }

// Get returns the value under key.
func (m Map) Get(key Value) (Value, bool) {
	if keyRank(key) < 0 {
		return nil, false
	}
	i, ok := m.find(key)
	if !ok {
		return nil, false
	}
	return m.entries[i].Val, true
}

// With returns m with key set to val.
func (m Map) With(key, val Value) (Map, error) {
	if keyRank(key) < 0 {
		return Map{}, fmt.Errorf("map key %s: must be a bool, int, string or keyword", Print(key))
	}
	es := slices.Clone(m.entries)
	if i, ok := m.find(key); ok {
		es[i].Val = val
	} else {
		es = slices.Insert(es, i, Entry{key, val})
	}
	return Map{es}, nil
}

// find returns where key is, or would go, in m's entries.
func (m Map) find(key Value) (int, bool) {
	return slices.BinarySearchFunc(m.entries, key, func(e Entry, k Value) int {
		return compareKeys(e.Key, k)
	})
}

// keyRank orders map keys by kind: bools, ints, strings, keywords. It is -1
// for a value that can't be a key.
func keyRank(v Value) int {
	switch v.(type) {
	case Bool:
		return 0
	case Int:
		return 1
	case String:
		return 2
	case Keyword:
		return 3
	}
	return -1
}

// compareKeys orders map keys: by kind, then #f before #t, ints by value,
// strings and keywords bytewise.
func compareKeys(a, b Value) int {
	if c := cmp.Compare(keyRank(a), keyRank(b)); c != 0 {
		return c
	}
	switch a := a.(type) {
	case Bool:
		if a == b.(Bool) {
			return 0
		}
		if !a {
			return -1
		}
		return 1
	case Int:
		return cmp.Compare(a, b.(Int))
	case String:
		return cmp.Compare(a, b.(String))
	case Keyword:
		return cmp.Compare(a, b.(Keyword))
	}
	panic(fmt.Sprintf("puff: %T is not a map key", a))
}

// Tag is a named tuple: (ok 1), (none), (circle 2.0). Parens in data are
// always tags.
type Tag struct {
	name string
	args []Value
}

// NewTag returns the tag name with a copy of args.
func NewTag(name string, args ...Value) Tag { return Tag{name, slices.Clone(args)} }

func (t Tag) Name() string   { return t.name }
func (t Tag) Len() int       { return len(t.args) }
func (t Tag) At(i int) Value { return t.args[i] }

// Equal reports whether a and b are the same value. Values of different
// types are never equal, so (= 1 1.0) is #f.
func Equal(a, b Value) bool {
	switch a := a.(type) {
	case Vector:
		b, ok := b.(Vector)
		return ok && slices.EqualFunc(a.items, b.items, Equal)
	case Map:
		b, ok := b.(Map)
		return ok && slices.EqualFunc(a.entries, b.entries, func(x, y Entry) bool {
			return Equal(x.Key, y.Key) && Equal(x.Val, y.Val)
		})
	case Tag:
		b, ok := b.(Tag)
		return ok && a.name == b.name && slices.EqualFunc(a.args, b.args, Equal)
	}
	return a == b
}
