package puff

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// str1, str2 and str3 make builtins of functions of one, two or three
// strings.
func str1(f func(s string) Value) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		s, err := arg[String](args, 0, "a string")
		if err != nil {
			return nil, err
		}
		return f(string(s)), nil
	}
}

func str2(f func(a, b string) Value) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		ss, err := strs(args)
		if err != nil {
			return nil, err
		}
		return f(ss[0], ss[1]), nil
	}
}

func str3(f func(a, b, c string) Value) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		ss, err := strs(args)
		if err != nil {
			return nil, err
		}
		return f(ss[0], ss[1], ss[2]), nil
	}
}

func strs(vs []Value) ([]string, error) {
	ss := make([]string, len(vs))
	for i := range vs {
		s, err := arg[String](vs, i, "a string")
		if err != nil {
			return nil, err
		}
		ss[i] = string(s)
	}
	return ss, nil
}

var stringFns = []*Builtin{
	{Name: "string/length", Args: "s", Doc: "The number of characters in s.", Fn: str1(func(s string) Value {
		return Int(utf8.RuneCountInString(s))
	})},
	{Name: "string/concat", Args: "s ...", Doc: "The strings joined together.", Fn: func(_ *Call, args []Value) (Value, error) {
		ss, err := strs(args)
		if err != nil {
			return nil, err
		}
		return String(strings.Join(ss, "")), nil
	}},
	{Name: "str", Args: "v ...", Doc: "The values joined together, strings as they are and the rest printed: (str \"line \" 12) is \"line 12\".", Fn: func(_ *Call, args []Value) (Value, error) {
		var b strings.Builder
		for _, v := range args {
			b.WriteString(display(v))
		}
		return String(b.String()), nil
	}},
	{Name: "string/split", Args: "s sep", Doc: "The parts of s around each sep.", Fn: str2(func(s, sep string) Value {
		return strVec(strings.Split(s, sep))
	})},
	{Name: "string/join", Args: "parts sep", Doc: "The strings in parts, with sep between them.", Fn: func(_ *Call, args []Value) (Value, error) {
		parts, err := arg[Vector](args, 0, "a vector")
		if err != nil {
			return nil, err
		}
		ss, err := strs(parts.items)
		if err != nil {
			return nil, err
		}
		sep, err := arg[String](args, 1, "a string")
		if err != nil {
			return nil, err
		}
		return String(strings.Join(ss, string(sep))), nil
	}},
	{Name: "string/lines", Args: "s", Doc: "The lines of s, without their line endings.", Fn: str1(func(s string) Value {
		lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
		if s == "" {
			lines = nil
		}
		for i, l := range lines {
			lines[i] = strings.TrimSuffix(l, "\r")
		}
		return strVec(lines)
	})},
	{Name: "string/trim", Args: "s", Doc: "s without white space at either end.", Fn: str1(func(s string) Value {
		return String(strings.TrimSpace(s))
	})},
	{Name: "string/upper", Args: "s", Doc: "s in upper case.", Fn: str1(func(s string) Value { return String(strings.ToUpper(s)) })},
	{Name: "string/lower", Args: "s", Doc: "s in lower case.", Fn: str1(func(s string) Value { return String(strings.ToLower(s)) })},
	{Name: "string/contains?", Args: "s sub", Doc: "Whether sub is in s.", Fn: str2(func(s, sub string) Value {
		return Bool(strings.Contains(s, sub))
	})},
	{Name: "string/starts-with?", Args: "s prefix", Doc: "Whether s starts with prefix.", Fn: str2(func(s, p string) Value {
		return Bool(strings.HasPrefix(s, p))
	})},
	{Name: "string/ends-with?", Args: "s suffix", Doc: "Whether s ends with suffix.", Fn: str2(func(s, p string) Value {
		return Bool(strings.HasSuffix(s, p))
	})},
	{Name: "string/replace", Args: "s old new", Doc: "s with every old replaced by new.", Fn: str3(func(s, old, new string) Value {
		return String(strings.ReplaceAll(s, old, new))
	})},
	{Name: "string/replace-first", Args: "s old new", Doc: "s with the first old replaced by new.", Fn: str3(func(s, old, new string) Value {
		return String(strings.Replace(s, old, new, 1))
	})},
	{Name: "string/trim-prefix", Args: "s prefix", Doc: "s without prefix at its start; s if it doesn't start with it.", Fn: str2(func(s, p string) Value {
		return String(strings.TrimPrefix(s, p))
	})},
	{Name: "string/trim-suffix", Args: "s suffix", Doc: "s without suffix at its end; s if it doesn't end with it.", Fn: str2(func(s, p string) Value {
		return String(strings.TrimSuffix(s, p))
	})},
	{Name: "string/cut", Args: "s sep", Doc: "(some [before after]): s around the first sep; (none) if sep isn't in s.", Fn: str2(func(s, sep string) Value {
		before, after, ok := strings.Cut(s, sep)
		if !ok {
			return None()
		}
		return Some(strVec([]string{before, after}))
	})},
	{Name: "string/index", Args: "s sub", Doc: "(some i): where sub first is in s, counting characters; (none) if it isn't.", Fn: str2(func(s, sub string) Value {
		i := strings.Index(s, sub)
		if i < 0 {
			return None()
		}
		return Some(Int(utf8.RuneCountInString(s[:i])))
	})},
	{Name: "string/slice", Args: "s start [end]", Doc: "The characters of s from start up to end, or to the end of s.", Fn: func(_ *Call, args []Value) (Value, error) {
		s, err := arg[String](args, 0, "a string")
		if err != nil {
			return nil, err
		}
		rs := []rune(string(s))
		start, end, err := span(args, 1, len(rs))
		if err != nil {
			return nil, err
		}
		return String(rs[start:end]), nil
	}},
}

func int2(f func(a, b int64) (Value, error)) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		a, err := arg[Int](args, 0, "an int")
		if err != nil {
			return nil, err
		}
		b, err := arg[Int](args, 1, "an int")
		if err != nil {
			return nil, err
		}
		return f(int64(a), int64(b))
	}
}

func float1(f func(x float64) Value) builtinFn {
	return func(_ *Call, args []Value) (Value, error) {
		x, err := arg[Float](args, 0, "a float")
		if err != nil {
			return nil, err
		}
		return f(float64(x)), nil
	}
}

var numberFns = []*Builtin{
	{Name: "int/from-string", Args: "s", Doc: "(ok n): s read as an int.", Fn: str1(func(s string) Value {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Errorf("parse", "not an int: %q", s)
		}
		return Ok(Int(n))
	})},
	{Name: "int/to-string", Args: "n", Doc: "n in decimal.", Fn: func(_ *Call, args []Value) (Value, error) {
		n, err := arg[Int](args, 0, "an int")
		if err != nil {
			return nil, err
		}
		return String(strconv.FormatInt(int64(n), 10)), nil
	}},
	{Name: "int/from-float", Args: "x", Doc: "(ok n): x without its fraction.", Fn: float1(func(x float64) Value {
		t := math.Trunc(x)
		if t < math.MinInt64 || t >= math.MaxInt64 {
			return Errorf("range", "%s is out of range for an int", Print(Float(x)))
		}
		return Ok(Int(int64(t)))
	})},
	{Name: "int/div", Args: "a b", Doc: "a divided by b, without the remainder.", Fn: int2(func(a, b int64) (Value, error) {
		if b == 0 {
			return nil, errors.New("division by zero")
		}
		if a == math.MinInt64 && b == -1 {
			return nil, errors.New("int overflow")
		}
		return Int(a / b), nil
	})},
	{Name: "int/rem", Args: "a b", Doc: "The remainder of a divided by b.", Fn: int2(func(a, b int64) (Value, error) {
		if b == 0 {
			return nil, errors.New("division by zero")
		}
		return Int(a % b), nil
	})},
	{Name: "float/from-string", Args: "s", Doc: "(ok x): s read as a float.", Fn: str1(func(s string) Value {
		x, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(x, 0) || math.IsNaN(x) {
			return Errorf("parse", "not a float: %q", s)
		}
		return Ok(Float(x))
	})},
	{Name: "float/to-string", Args: "x", Doc: "x as puff prints it.", Fn: float1(func(x float64) Value { return String(Print(Float(x))) })},
	{Name: "float/from-int", Args: "n", Doc: "n as a float.", Fn: func(_ *Call, args []Value) (Value, error) {
		n, err := arg[Int](args, 0, "an int")
		if err != nil {
			return nil, err
		}
		return Float(n), nil
	}},
	{Name: "float/div", Args: "a b", Doc: "a divided by b.", Fn: func(_ *Call, args []Value) (Value, error) {
		a, err := arg[Float](args, 0, "a float")
		if err != nil {
			return nil, err
		}
		b, err := arg[Float](args, 1, "a float")
		if err != nil {
			return nil, err
		}
		if b == 0 {
			return nil, errors.New("division by zero")
		}
		x := float64(a / b)
		if math.IsInf(x, 0) {
			return nil, errors.New("float overflow")
		}
		return Float(x), nil
	}},
	{Name: "float/round", Args: "x", Doc: "x rounded to the nearest whole number, halves away from zero.", Fn: float1(func(x float64) Value { return Float(math.Round(x)) })},
	{Name: "float/floor", Args: "x", Doc: "The greatest whole number at most x.", Fn: float1(func(x float64) Value { return Float(math.Floor(x)) })},
	{Name: "float/ceil", Args: "x", Doc: "The least whole number at least x.", Fn: float1(func(x float64) Value { return Float(math.Ceil(x)) })},
}

var valueFns = []*Builtin{
	{Name: "value/to-string", Args: "v", Doc: "v printed as puff reads it.", Fn: func(_ *Call, args []Value) (Value, error) {
		return String(Print(args[0])), nil
	}},
	{Name: "value/from-string", Args: "s", Doc: "(ok v): s read as a value.", Fn: str1(func(s string) Value {
		v, err := ReadValue(s)
		if err != nil {
			return Errorf("parse", "%s", err)
		}
		return Ok(v)
	})},
	{Name: "value/type", Args: "v", Doc: "v's type: :bool, :int, :float, :string, :keyword, :duration, :vector, :map, :tag or :function.", Fn: func(_ *Call, args []Value) (Value, error) {
		return Keyword(typeName(args[0])), nil
	}},
	{Name: "keyword/name", Args: "k", Doc: "k's name, without its colon: \"ready\" for :ready.", Fn: func(_ *Call, args []Value) (Value, error) {
		k, err := arg[Keyword](args, 0, "a keyword")
		if err != nil {
			return nil, err
		}
		return String(k), nil
	}},
}

// display is v as str and io/println show it: a string as it is, anything
// else printed.
func display(v Value) string {
	if s, ok := v.(String); ok {
		return string(s)
	}
	return Print(v)
}
