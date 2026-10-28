package puff

import (
	"fmt"
	"slices"
	"strings"
)

// Format lays src, a puff file, out in one way, so that who wrote it and
// how doesn't show in a diff. It keeps the line breaks and every token as
// written: strings, """ strings, numbers, map keys in their order and
// comments. What it decides is the rest:
//
//   - A line inside ( ) is indented 2 past the (; one inside [ ] or { }
//     lines up with the first element, 1 past the bracket. The rule
//     doesn't look at what the form is, so renaming a function doesn't
//     move its arguments.
//   - Closing brackets on a line of their own join the line before,
//     unless that ends in a comment.
//   - One space between tokens, none inside brackets; a comment after
//     code keeps the space before it.
//   - At most one blank line in a row, none at the start or end, and one
//     newline at the end of the file.
//
// A """ string's lines move with the line it starts on, which keeps its
// value. A file that doesn't read is an error at file:line:col.
func Format(file, src string) (string, error) {
	before, err := Read(file, src)
	if err != nil {
		return "", err
	}
	out := layout(tokens(file, src))
	after, err := Read(file, out)
	if err != nil || !sameForms(before, after) {
		return "", fmt.Errorf("%s: formatting would change the code; this is a bug in puff fmt", file)
	}
	return out, nil
}

// token is a piece of source fmt keeps as it is.
type token struct {
	text            string
	kind            byte // '(' for an opening bracket, ')' for a closing one, ';' for a comment, 'a' for the rest
	line, col       int  // where it starts
	endLine, endCol int  // just after it: a string can end on a later line
}

// tokens splits src, which reads, into tokens.
func tokens(file, src string) []token {
	r := &reader{file: file, src: src, line: 1, col: 1}
	var ts []token
	for {
		for !r.eof() && strings.IndexByte(" \t\r\n", r.peek()) >= 0 {
			r.next()
		}
		if r.eof() {
			return ts
		}
		p, start := r.pos(), r.off
		t := token{kind: 'a', line: p.Line, col: p.Col}
		switch c := r.peek(); {
		case c == ';':
			for !r.eof() && r.peek() != '\n' {
				r.next()
			}
			t.kind = ';'
		case strings.IndexByte("([{", c) >= 0:
			r.next()
			t.kind = '('
		case strings.IndexByte(")]}", c) >= 0:
			r.next()
			t.kind = ')'
		case strings.HasPrefix(r.src[r.off:], `"""`):
			r.text(p)
		case c == '"':
			r.str(p)
		default:
			r.atom(p)
		}
		t.text = src[start:r.off]
		if t.kind == ';' {
			t.text = strings.TrimRight(t.text, " \t\r")
		}
		t.endLine, t.endCol = r.line, r.col
		ts = append(ts, t)
	}
}

// bracket is an opening bracket in the output, still open.
type bracket struct {
	c   byte // (, [ or {
	col int  // its column, from 0
}

// layout writes ts out by Format's rules.
func layout(ts []token) string {
	var b strings.Builder
	var open []bracket
	col := 0   // the output column, from 0
	shift := 0 // how far the current line moved from where it was
	endLine := func() {
		s := strings.TrimRight(b.String(), " ")
		b.Reset()
		b.WriteString(s)
		b.WriteByte('\n')
	}
	for i, t := range ts {
		var prev token
		if i > 0 {
			prev = ts[i-1]
		}
		newLine := i > 0 && t.line > prev.endLine && !(t.kind == ')' && prev.kind != ';')
		switch {
		case newLine:
			endLine()
			if t.line > prev.endLine+1 {
				b.WriteByte('\n')
			}
			n := indent(open, t)
			b.WriteString(strings.Repeat(" ", n))
			col, shift = n, n-(t.col-1)
		case i > 0 && (t.kind == ';' || prev.kind != '(' && t.kind != ')'):
			gap := 1
			if t.kind == ';' && t.line == prev.endLine {
				gap = max(1, t.col-prev.endCol)
			}
			b.WriteString(strings.Repeat(" ", gap))
			col += gap
		}
		text := t.text
		if strings.HasPrefix(text, `"""`) {
			text = shiftText(text, shift)
		}
		switch t.kind {
		case '(':
			open = append(open, bracket{text[0], col})
		case ')':
			open = open[:len(open)-1]
		}
		b.WriteString(text)
		if j := strings.LastIndexByte(text, '\n'); j >= 0 {
			col = len(text) - j - 1
		} else {
			col += len(text)
		}
	}
	if len(ts) > 0 {
		endLine()
	}
	return b.String()
}

// indent is the indent of a line that starts with t, inside open.
func indent(open []bracket, t token) int {
	if len(open) == 0 {
		return 0
	}
	in := open[len(open)-1]
	switch {
	case t.kind == ')': // after a comment
		return in.col
	case in.c == '(':
		return in.col + 2
	}
	return in.col + 1
}

// shiftText moves the lines of a """ string after its first by n columns,
// which keeps its value: what counts is their indent past the closing
// """'s. A string whose lines can't move that far (tabs, say) stays as it
// is, which keeps its value too.
func shiftText(text string, n int) string {
	if n == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.TrimSpace(l) == "":
			lines[i] = ""
		case n > 0:
			lines[i] = strings.Repeat(" ", n) + l
		case strings.HasPrefix(l, strings.Repeat(" ", -n)):
			lines[i] = l[-n:]
		default:
			return text
		}
	}
	return strings.Join(lines, "\n")
}

// sameForms says whether a and b are the same code.
func sameForms(a, b []*Form) bool {
	return slices.EqualFunc(a, b, func(x, y *Form) bool { return x.String() == y.String() })
}
