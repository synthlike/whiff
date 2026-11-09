package puff

import (
	"strings"
	"unicode/utf8"
)

// PrintPretty is v as Print has it, broken over lines to fit in width
// columns where it can, for a person to read: a vector, map or tag that
// doesn't fit on what is left of its line has an item, entry or argument a
// line (a vector of atoms, as many as fit), indented as puff -f indents,
// 1 past a [ or { and 2 past a (. A value that fits stays on one line, and
// so does an atom too long for width. col is the column v starts at, for
// the lines after the first.
func PrintPretty(v Value, col, width int) string {
	var b strings.Builder
	pretty(&b, v, col, width)
	return b.String()
}

func pretty(b *strings.Builder, v Value, col, width int) {
	flat := Print(v)
	if col+utf8.RuneCountInString(flat) <= width {
		b.WriteString(flat)
		return
	}
	switch v := v.(type) {
	case Vector:
		b.WriteByte('[')
		if atoms(v.items) {
			// Atoms, as a command's argv, fill each line.
			at := col + 1
			for i, item := range v.items {
				p := Print(item)
				n := utf8.RuneCountInString(p)
				if i == len(v.items)-1 {
					n++ // and the ]
				}
				if i > 0 && at+1+n > width {
					newline(b, col+1)
					at = col + 1
				} else if i > 0 {
					b.WriteByte(' ')
					at++
				}
				b.WriteString(p)
				at += n
			}
			b.WriteByte(']')
			return
		}
		for i, item := range v.items {
			if i > 0 {
				newline(b, col+1)
			}
			pretty(b, item, col+1, width)
		}
		b.WriteByte(']')
	case Map:
		b.WriteByte('{')
		for i, e := range v.entries {
			if i > 0 {
				newline(b, col+1)
			}
			key := Print(e.Key)
			b.WriteString(key)
			b.WriteByte(' ')
			pretty(b, e.Val, col+1+utf8.RuneCountInString(key)+1, width)
		}
		b.WriteByte('}')
	case Tag:
		b.WriteByte('(')
		b.WriteString(v.name)
		for i, arg := range v.args {
			if i == 0 {
				b.WriteByte(' ')
				pretty(b, arg, col+1+utf8.RuneCountInString(v.name)+1, width)
				continue
			}
			newline(b, col+2)
			pretty(b, arg, col+2, width)
		}
		b.WriteByte(')')
	default:
		b.WriteString(flat)
	}
}

// atoms says whether none of vs is a vector, map or tag.
func atoms(vs []Value) bool {
	for _, v := range vs {
		switch v.(type) {
		case Vector, Map, Tag:
			return false
		}
	}
	return true
}

func newline(b *strings.Builder, col int) {
	b.WriteByte('\n')
	b.WriteString(strings.Repeat(" ", col))
}
