package puff

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// regexps caches compiled patterns by their source; patterns are nearly
// always literals, so it stays small.
var regexps sync.Map

func regex(args []Value, i int) (*regexp.Regexp, error) {
	p, err := arg[String](args, i, "a regex string")
	if err != nil {
		return nil, err
	}
	if re, ok := regexps.Load(string(p)); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(string(p))
	if err != nil {
		return nil, fmt.Errorf("bad regex: %v", err)
	}
	regexps.Store(string(p), re)
	return re, nil
}

// onRegex makes a builtin of a function of a string and a regex.
func onRegex(f func(s string, re *regexp.Regexp, args []Value) (Value, error)) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		s, err := arg[String](args, 0, "a string")
		if err != nil {
			return nil, err
		}
		re, err := regex(args, 1)
		if err != nil {
			return nil, err
		}
		return f(string(s), re, args)
	}
}

var regexFns = []*Builtin{
	{Name: "regex/match?", Args: "s re", Doc: "Whether re matches somewhere in s. Patterns are Go's RE2 syntax.", Fn: onRegex(func(s string, re *regexp.Regexp, _ []Value) (Value, error) {
		return Bool(re.MatchString(s)), nil
	})},
	{Name: "regex/find", Args: "s re", Doc: "(some m): the first match of re in s; (none) if there isn't one.", Fn: onRegex(func(s string, re *regexp.Regexp, _ []Value) (Value, error) {
		loc := re.FindStringIndex(s)
		if loc == nil {
			return None(), nil
		}
		return Some(String(s[loc[0]:loc[1]])), nil
	})},
	{Name: "regex/find-all", Args: "s re", Doc: "Every match of re in s.", Fn: onRegex(func(s string, re *regexp.Regexp, _ []Value) (Value, error) {
		return strVec(re.FindAllString(s, -1)), nil
	})},
	{Name: "regex/groups", Args: "s re", Doc: "(some [g1 g2 ...]): the groups of the first match; (none) if there isn't one.", Fn: onRegex(func(s string, re *regexp.Regexp, _ []Value) (Value, error) {
		m := re.FindStringSubmatch(s)
		if m == nil {
			return None(), nil
		}
		return Some(strVec(m[1:])), nil
	})},
	{Name: "regex/replace", Args: "s re new", Doc: "s with every match of re replaced by new, where $1 is a group.", Fn: onRegex(func(s string, re *regexp.Regexp, args []Value) (Value, error) {
		repl, err := arg[String](args, 2, "a string")
		if err != nil {
			return nil, err
		}
		return String(re.ReplaceAllString(s, string(repl))), nil
	})},
	{Name: "regex/split", Args: "s re", Doc: "The parts of s around each match of re.", Fn: onRegex(func(s string, re *regexp.Regexp, _ []Value) (Value, error) {
		return strVec(re.Split(s, -1)), nil
	})},
}

// jsonNull is JSON's null: the tag (null).
var jsonNull = NewTag("null")

var jsonFns = []*Builtin{
	{Name: "json/parse", Args: "s [opts]", Doc: "(ok v): s read as JSON. Objects are maps, with keyword keys where the key is a name; null is (null); whole numbers that fit stay ints.", Fn: func(_ *Call, args []Value) (Value, error) {
		s, err := arg[String](args, 0, "a string")
		if err != nil {
			return nil, err
		}
		if _, err := options(args, 1); err != nil {
			return nil, err
		}
		v, err := ParseJSON(string(s))
		if err != nil {
			return Errorf("parse", "%s", err), nil
		}
		return Ok(v), nil
	}},
	{Name: "json/to-string", Args: "v [opts]", Doc: "v as JSON; {:indent n} indents by n spaces.", Fn: func(_ *Call, args []Value) (Value, error) {
		opts, err := options(args, 1, "indent")
		if err != nil {
			return nil, err
		}
		x, err := toJSON(args[0])
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if v, ok := opts.Get(Keyword("indent")); ok {
			n, ok := v.(Int)
			if !ok || n < 0 || n > 16 {
				return nil, fmt.Errorf("want :indent from 0 to 16, got %s", Print(v))
			}
			enc.SetIndent("", strings.Repeat(" ", int(n)))
		}
		if err := enc.Encode(x); err != nil {
			return nil, err
		}
		return String(strings.TrimSuffix(b.String(), "\n")), nil
	}},
	{Name: "json/null", Doc: "JSON's null: (null).", Fn: func(*Call, []Value) (Value, error) { return jsonNull, nil }},
}

// ParseJSON reads s, one JSON value, as json/parse does: objects are
// maps, with keyword keys where the key is a name; null is (null); whole
// numbers that fit stay ints.
func ParseJSON(s string) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("extra data after the value")
	}
	return fromJSON(x)
}

func fromJSON(x any) (Value, error) {
	switch x := x.(type) {
	case nil:
		return jsonNull, nil
	case bool:
		return Bool(x), nil
	case string:
		return String(x), nil
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return Int(n), nil
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return nil, fmt.Errorf("number %s is out of range", x)
		}
		return Float(f), nil
	case []any:
		items := make([]Value, len(x))
		for i, it := range x {
			v, err := fromJSON(it)
			if err != nil {
				return nil, err
			}
			items[i] = v
		}
		return Vector{items}, nil
	case map[string]any:
		es := make([]Entry, 0, len(x))
		for k, it := range x {
			v, err := fromJSON(it)
			if err != nil {
				return nil, err
			}
			var key Value = String(k)
			if isName(k) {
				key = Keyword(k)
			}
			es = append(es, Entry{key, v})
		}
		return NewMap(es...)
	}
	return nil, fmt.Errorf("unexpected JSON value %v", x)
}

func toJSON(v Value) (any, error) {
	switch v := v.(type) {
	case Bool:
		return bool(v), nil
	case Int:
		return int64(v), nil
	case Float:
		return float64(v), nil
	case String:
		return string(v), nil
	case Keyword:
		return string(v), nil
	case Vector:
		out := make([]any, len(v.items))
		for i, it := range v.items {
			x, err := toJSON(it)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case Map:
		out := make(map[string]any, len(v.entries))
		for _, e := range v.entries {
			var k string
			switch key := e.Key.(type) {
			case Bool, Int:
				k = Print(key)
			case String:
				k = string(key)
			case Keyword:
				k = string(key)
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("key %s appears twice", k)
			}
			x, err := toJSON(e.Val)
			if err != nil {
				return nil, err
			}
			out[k] = x
		}
		return out, nil
	case Tag:
		if Equal(v, jsonNull) {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("can't encode %s %s as JSON", typeName(v), Print(v))
}
