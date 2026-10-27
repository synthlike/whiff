package puff

import (
	"strconv"
	"strings"
	"time"
)

// Print returns v's readable form: reading it back gives a value Equal to v,
// and Equal values print the same.
func Print(v Value) string {
	var b strings.Builder
	v.print(&b)
	return b.String()
}

func (v Bool) print(b *strings.Builder) {
	if v {
		b.WriteString("#t")
	} else {
		b.WriteString("#f")
	}
}

func (v Int) print(b *strings.Builder) { b.WriteString(strconv.FormatInt(int64(v), 10)) }

func (v Float) print(b *strings.Builder) {
	s := strconv.FormatFloat(float64(v), 'g', -1, 64)
	// 1 prints as 1.0 so it reads back as a float; 1e+21, NaN and Inf
	// already don't look like ints.
	if !strings.ContainsAny(s, ".eNI") {
		s += ".0"
	}
	b.WriteString(s)
}

// Strings use Go's quoting and escapes.
func (v String) print(b *strings.Builder) { b.WriteString(strconv.Quote(string(v))) }

func (v Keyword) print(b *strings.Builder) {
	b.WriteByte(':')
	b.WriteString(string(v))
}

// durationUnits are the duration suffixes, largest first.
var durationUnits = []struct {
	name string
	d    time.Duration
}{
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
	{"us", time.Microsecond},
	{"ns", time.Nanosecond},
}

// Durations print in the largest unit that divides them: 90s, 2d, 1500us.
func (v Duration) print(b *strings.Builder) {
	d := time.Duration(v)
	if d == 0 {
		b.WriteString("0s")
		return
	}
	for _, u := range durationUnits {
		if d%u.d == 0 {
			b.WriteString(strconv.FormatInt(int64(d/u.d), 10))
			b.WriteString(u.name)
			return
		}
	}
}

// Times print in RFC 3339 in UTC, with the fractional digits they need:
// 2026-10-27T10:00:00Z, 2026-10-27T10:00:00.5Z.
func (v Time) print(b *strings.Builder) { b.WriteString(v.Go().Format(time.RFC3339Nano)) }

func (v Vector) print(b *strings.Builder) {
	b.WriteByte('[')
	printSeq(b, v.items)
	b.WriteByte(']')
}

func (v Map) print(b *strings.Builder) {
	b.WriteByte('{')
	for i, e := range v.entries {
		if i > 0 {
			b.WriteByte(' ')
		}
		e.Key.print(b)
		b.WriteByte(' ')
		e.Val.print(b)
	}
	b.WriteByte('}')
}

func (v Tag) print(b *strings.Builder) {
	b.WriteByte('(')
	b.WriteString(v.name)
	if len(v.args) > 0 {
		b.WriteByte(' ')
		printSeq(b, v.args)
	}
	b.WriteByte(')')
}

func printSeq(b *strings.Builder, vs []Value) {
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(' ')
		}
		v.print(b)
	}
}
