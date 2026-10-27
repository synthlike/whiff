package puff

import (
	"testing"
	"time"
)

func mustMap(t *testing.T, kvs ...Value) Map {
	t.Helper()
	var es []Entry
	for i := 0; i < len(kvs); i += 2 {
		es = append(es, Entry{kvs[i], kvs[i+1]})
	}
	m, err := NewMap(es...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPrint(t *testing.T) {
	tests := []struct {
		v    Value
		want string
	}{
		{Bool(true), "#t"},
		{Bool(false), "#f"},
		{Int(-42), "-42"},
		{Float(1), "1.0"},
		{Float(-1.5), "-1.5"},
		{Float(1e21), "1e+21"},
		{String("a \"b\"\n"), `"a \"b\"\n"`},
		{Keyword("gh/prs"), ":gh/prs"},
		{Duration(0), "0s"},
		{Duration(500 * time.Millisecond), "500ms"},
		{Duration(90 * time.Second), "90s"},
		{Duration(48 * time.Hour), "2d"},
		{Duration(1500 * time.Microsecond), "1500us"},
		{Duration(-5 * time.Second), "-5s"},
		{NewVector(), "[]"},
		{NewVector(Int(1), String("a"), NewVector()), `[1 "a" []]`},
		{mustMap(t), "{}"},
		{NewTag("none"), "(none)"},
		{NewTag("ok", NewVector(Int(1))), "(ok [1])"},
		{NewTag("rect", Float(2), Float(3)), "(rect 2.0 3.0)"},
	}
	for _, tt := range tests {
		if got := Print(tt.v); got != tt.want {
			t.Errorf("Print(%#v) = %s, want %s", tt.v, got, tt.want)
		}
	}
}

func TestMapPrintOrder(t *testing.T) {
	m := mustMap(t,
		Keyword("b"), Int(1),
		String("a"), Int(2),
		Int(10), Int(3),
		Bool(true), Int(4),
		Keyword("a"), Int(5),
		Int(-1), Int(6),
		Bool(false), Int(7),
	)
	want := `{#f 7 #t 4 -1 6 10 3 "a" 2 :a 5 :b 1}`
	if got := Print(m); got != want {
		t.Errorf("Print = %s, want %s", got, want)
	}
}

func TestNewMapErrors(t *testing.T) {
	tests := []struct {
		name string
		es   []Entry
	}{
		{"duplicate", []Entry{{Int(1), Int(1)}, {Keyword("a"), Int(2)}, {Int(1), Int(3)}}},
		{"float key", []Entry{{Float(1), Int(1)}}},
		{"duration key", []Entry{{Duration(time.Second), Int(1)}}},
		{"vector key", []Entry{{NewVector(), Int(1)}}},
	}
	for _, tt := range tests {
		if m, err := NewMap(tt.es...); err == nil {
			t.Errorf("%s: NewMap = %s, want an error", tt.name, Print(m))
		}
	}
}

func TestMapGet(t *testing.T) {
	m := mustMap(t, Keyword("a"), Int(1), String("a"), Int(2), Int(1), Int(3))
	tests := []struct {
		key  Value
		want Value // nil: absent
	}{
		{Keyword("a"), Int(1)},
		{String("a"), Int(2)},
		{Int(1), Int(3)},
		{Keyword("b"), nil},
		{Bool(true), nil},
		{Float(1), nil},
	}
	for _, tt := range tests {
		got, ok := m.Get(tt.key)
		if ok != (tt.want != nil) || ok && !Equal(got, tt.want) {
			t.Errorf("Get(%s) = %v, %v; want %v", Print(tt.key), got, ok, tt.want)
		}
	}
}

func TestEqual(t *testing.T) {
	tests := []struct {
		a, b Value
		want bool
	}{
		{Int(1), Int(1), true},
		{Int(1), Float(1), false},
		{String("a"), Keyword("a"), false},
		{Duration(time.Minute), Duration(60 * time.Second), true},
		{NewVector(Int(1), NewVector(Int(2))), NewVector(Int(1), NewVector(Int(2))), true},
		{NewVector(Int(1)), NewVector(Int(1), Int(2)), false},
		{NewVector(), NewTag("v"), false},
		{mustMap(t, Keyword("a"), Int(1), Int(2), Int(3)), mustMap(t, Int(2), Int(3), Keyword("a"), Int(1)), true},
		{mustMap(t, Keyword("a"), Int(1)), mustMap(t, Keyword("a"), Float(1)), false},
		{NewTag("ok", Int(1)), NewTag("ok", Int(1)), true},
		{NewTag("ok", Int(1)), NewTag("err", Int(1)), false},
		{NewTag("none"), NewTag("none"), true},
	}
	for _, tt := range tests {
		if got := Equal(tt.a, tt.b); got != tt.want {
			t.Errorf("Equal(%s, %s) = %v, want %v", Print(tt.a), Print(tt.b), got, tt.want)
		}
	}
}

func TestNewVectorCopies(t *testing.T) {
	items := []Value{Int(1)}
	v := NewVector(items...)
	items[0] = Int(2)
	if !Equal(v.At(0), Int(1)) {
		t.Errorf("vector changed with its input: %s", Print(v))
	}
}
