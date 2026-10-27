package puff

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Read reads every form in src. file names src in positions.
func Read(file, src string) ([]*Form, error) {
	r := &reader{file: file, src: src, line: 1, col: 1}
	var forms []*Form
	for {
		r.skip()
		if r.eof() {
			return forms, nil
		}
		f, err := r.form()
		if err != nil {
			return nil, err
		}
		forms = append(forms, f)
	}
}

// ReadValue reads src as exactly one value, such as a message or a printed
// value.
func ReadValue(src string) (Value, error) {
	forms, err := Read("", src)
	if err != nil {
		return nil, err
	}
	if len(forms) != 1 {
		p := Pos{Line: 1, Col: 1}
		if len(forms) > 1 {
			p = forms[1].Pos
		}
		return nil, errorf(p, "want one value, got %d", len(forms))
	}
	return forms[0].Value()
}

type reader struct {
	file      string
	src       string
	off       int
	line, col int
}

func (r *reader) pos() Pos   { return Pos{r.file, r.line, r.col} }
func (r *reader) eof() bool  { return r.off >= len(r.src) }
func (r *reader) peek() byte { return r.src[r.off] }

func (r *reader) next() {
	if r.src[r.off] == '\n' {
		r.line++
		r.col = 1
	} else {
		r.col++
	}
	r.off++
}

// skip skips whitespace and comments.
func (r *reader) skip() {
	for !r.eof() {
		switch r.peek() {
		case ' ', '\t', '\r', '\n':
			r.next()
		case ';':
			for !r.eof() && r.peek() != '\n' {
				r.next()
			}
		default:
			return
		}
	}
}

// form reads the form at the reader's position, which isn't at the end.
func (r *reader) form() (*Form, error) {
	p := r.pos()
	switch c := r.peek(); c {
	case '(':
		return r.seq(p, FormList, ')')
	case '[':
		return r.seq(p, FormVector, ']')
	case '{':
		f, err := r.seq(p, FormMap, '}')
		if err != nil {
			return nil, err
		}
		if err := checkMap(f); err != nil {
			return nil, err
		}
		return f, nil
	case ')', ']', '}':
		return nil, errorf(p, "unexpected %q", c)
	case '"':
		return r.str(p)
	}
	return r.atom(p)
}

func (r *reader) seq(p Pos, kind FormKind, close byte) (*Form, error) {
	r.next()
	f := &Form{Kind: kind, Pos: p}
	for {
		r.skip()
		if r.eof() {
			return nil, errorf(p, "no closing %q", close)
		}
		if r.peek() == close {
			r.next()
			return f, nil
		}
		item, err := r.form()
		if err != nil {
			return nil, err
		}
		f.Items = append(f.Items, item)
	}
}

// checkMap rejects a map literal with a key and no value, and literal keys
// that can't be keys or appear twice. Computed keys are checked when the map
// is built.
func checkMap(f *Form) error {
	if len(f.Items)%2 != 0 {
		return errorf(f.Pos, "map literal needs a value for every key")
	}
	seen := map[string]bool{}
	for i := 0; i < len(f.Items); i += 2 {
		k := f.Items[i]
		if k.Kind != FormLiteral {
			continue
		}
		s := Print(k.Val) // equal keys print the same
		if keyRank(k.Val) < 0 {
			return errorf(k.Pos, "map key %s: must be a bool, int, string or keyword", s)
		}
		if seen[s] {
			return errorf(k.Pos, "duplicate map key %s", s)
		}
		seen[s] = true
	}
	return nil
}

// Strings use Go's escapes and may span lines.
var lineBreaks = strings.NewReplacer("\n", `\n`, "\r", `\r`)

func (r *reader) str(p Pos) (*Form, error) {
	start := r.off
	r.next()
	for !r.eof() {
		c := r.peek()
		r.next()
		switch c {
		case '"':
			s, err := strconv.Unquote(lineBreaks.Replace(r.src[start:r.off]))
			if err != nil {
				return nil, errorf(p, "bad escape in string")
			}
			return &Form{Kind: FormLiteral, Pos: p, Val: String(s)}, nil
		case '\\':
			if !r.eof() {
				r.next()
			}
		}
	}
	return nil, errorf(p, "unclosed string")
}

// atom reads a token up to the next delimiter: a bool, keyword, number,
// duration or symbol.
func (r *reader) atom(p Pos) (*Form, error) {
	start := r.off
	for !r.eof() && strings.IndexByte(" \t\r\n()[]{}\";", r.peek()) < 0 {
		r.next()
	}
	tok := r.src[start:r.off]
	lit := func(v Value) (*Form, error) { return &Form{Kind: FormLiteral, Pos: p, Val: v}, nil }
	switch {
	case tok == "#t":
		return lit(Bool(true))
	case tok == "#f":
		return lit(Bool(false))
	case tok[0] == '#':
		return nil, errorf(p, "unknown %s: booleans are #t and #f", tok)
	case tok[0] == ':':
		if !isName(tok[1:]) {
			return nil, errorf(p, "bad keyword %s", tok)
		}
		return lit(Keyword(tok[1:]))
	case isDigit(tok[0]) || len(tok) > 1 && (tok[0] == '-' || tok[0] == '+') && isDigit(tok[1]):
		v, err := number(tok)
		if err != nil {
			return nil, errorf(p, "%s", err)
		}
		return lit(v)
	case isName(tok):
		return &Form{Kind: FormSymbol, Pos: p, Name: tok}, nil
	}
	return nil, errorf(p, "bad name %s", tok)
}

var (
	intRe      = regexp.MustCompile(`^[+-]?[0-9]+$`)
	floatRe    = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	durationRe = regexp.MustCompile(`^([+-]?[0-9]+)([a-z]+)$`)
)

// number reads an int, float or duration token. Durations are whole numbers
// of one unit: 500ms, not 0.5s.
func number(tok string) (Value, error) {
	switch {
	case intRe.MatchString(tok):
		n, err := strconv.ParseInt(tok, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("int %s out of range", tok)
		}
		return Int(n), nil
	case floatRe.MatchString(tok):
		f, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			return nil, fmt.Errorf("float %s out of range", tok)
		}
		return Float(f), nil
	}
	if m := durationRe.FindStringSubmatch(tok); m != nil {
		for _, u := range durationUnits {
			if u.name != m[2] {
				continue
			}
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || n > math.MaxInt64/int64(u.d) || n < math.MinInt64/int64(u.d) {
				return nil, fmt.Errorf("duration %s out of range", tok)
			}
			return Duration(time.Duration(n) * u.d), nil
		}
	}
	return nil, fmt.Errorf("bad number %s", tok)
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isName reports whether s can name a symbol or keyword: ASCII letters,
// digits and -_/?!*+<>=.&%, not starting with a digit.
func isName(s string) bool {
	if s == "" || isDigit(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && !('a' <= c && c <= 'z') && !('A' <= c && c <= 'Z') &&
			strings.IndexByte("-_/?!*+<>=.&%", c) < 0 {
			return false
		}
	}
	return true
}
