package puff

import (
	"fmt"
	"strings"
)

// Pos is a position in source. Line and column count from 1; the column is
// in bytes.
type Pos struct {
	File      string
	Line, Col int
}

func (p Pos) String() string {
	if p.File == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Col)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Error is an error at a position in source.
type Error struct {
	Pos Pos
	Msg string
	// Unclosed is set when the source ends inside a form or string, so a
	// REPL can read another line instead.
	Unclosed bool
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

func errorf(p Pos, format string, args ...any) *Error {
	return &Error{Pos: p, Msg: fmt.Sprintf(format, args...)}
}

type FormKind int

const (
	FormLiteral FormKind = iota
	FormSymbol
	FormList
	FormVector
	FormMap
)

// Form is a piece of read source with its position. The reader doesn't tell
// code from data: Value turns a form into data, the evaluator reads it as
// code.
type Form struct {
	Kind  FormKind
	Pos   Pos
	Val   Value   // FormLiteral: a bool, int, float, string, keyword or duration
	Name  string  // FormSymbol
	Items []*Form // FormList, FormVector, FormMap (keys and values alternating)
}

// String prints f in normalized source form.
func (f *Form) String() string {
	switch f.Kind {
	case FormLiteral:
		return Print(f.Val)
	case FormSymbol:
		return f.Name
	}
	open, close := "(", ")"
	switch f.Kind {
	case FormVector:
		open, close = "[", "]"
	case FormMap:
		open, close = "{", "}"
	}
	var b strings.Builder
	b.WriteString(open)
	for i, it := range f.Items {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(it.String())
	}
	b.WriteString(close)
	return b.String()
}

// Value converts a form read as data: literals, vectors, maps, and lists as
// tags named by their first symbol. Other symbols are not data.
func (f *Form) Value() (Value, error) {
	switch f.Kind {
	case FormLiteral:
		return f.Val, nil
	case FormSymbol:
		return nil, errorf(f.Pos, "name %s in data", f.Name)
	case FormVector:
		items, err := formValues(f.Items)
		if err != nil {
			return nil, err
		}
		return NewVector(items...), nil
	case FormMap:
		items, err := formValues(f.Items)
		if err != nil {
			return nil, err
		}
		es := make([]Entry, 0, len(items)/2)
		for i := 0; i+1 < len(items); i += 2 {
			es = append(es, Entry{items[i], items[i+1]})
		}
		m, err := NewMap(es...)
		if err != nil {
			return nil, errorf(f.Pos, "%s", err)
		}
		return m, nil
	case FormList:
		if len(f.Items) == 0 || f.Items[0].Kind != FormSymbol {
			return nil, errorf(f.Pos, "a tag needs a name: (name args...)")
		}
		args, err := formValues(f.Items[1:])
		if err != nil {
			return nil, err
		}
		return NewTag(f.Items[0].Name, args...), nil
	}
	panic(fmt.Sprintf("puff: unknown form kind %d", f.Kind))
}

func formValues(fs []*Form) ([]Value, error) {
	vs := make([]Value, len(fs))
	for i, f := range fs {
		v, err := f.Value()
		if err != nil {
			return nil, err
		}
		vs[i] = v
	}
	return vs, nil
}
