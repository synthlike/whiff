package puff

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRead(t *testing.T) {
	tests := []struct {
		src  string
		want string // the forms' String, space-separated
	}{
		{"42", "42"},
		{"+3 -7", "3 -7"},
		{"1.5 1e3 -0.5", "1.5 1000.0 -0.5"},
		{`"a\tb" "x` + "\n" + `y"`, `"a\tb" "x\ny"`},
		{":gh/prs #t #f", ":gh/prs #t #f"},
		{"500ms 90s 1d -5s 7ns 60s", "500ms 90s 1d -5s 7ns 1m"},
		{"2026-10-27T10:00:00Z 2026-10-27T10:00:00.500Z", "2026-10-27T10:00:00Z 2026-10-27T10:00:00.5Z"},
		{"string/split -> - + & ok? int/from-string", "string/split -> - + & ok? int/from-string"},
		{"(f [a & rest] {:k 1})", "(f [a & rest] {:k 1})"},
		{"(a ; comment\n b)", "(a b)"},
		{"{:b 1 :a 2}", "{:b 1 :a 2}"},
		{"{(f) 1 (f) 2}", "{(f) 1 (f) 2}"},
		{"()  []\n{}", "() [] {}"},
		{"  ; only a comment\n", ""},
		// """ strings: raw, with the closing """'s indent taken off.
		{"(def p \"\"\"\n    Review \\n \"this\".\n      Skip nits.\n\n    \"\"\") x", `(def p "Review \\n \"this\".\n  Skip nits.\n") x`},
		{"\"\"\"  \r\none\r\n\"\"\"", `"one"`},
		{"\"\"\"\n\"\"\"", `""`},
		{"[\"\"\"\n  a\n\n  b\n  \"\"\" 1]", `["a\n\nb" 1]`},
		{"\"\"\"\n  a\n    \n  \"\"\"", `"a\n  "`},
	}
	for _, tt := range tests {
		forms, err := Read("t.puff", tt.src)
		if err != nil {
			t.Errorf("Read(%q): %v", tt.src, err)
			continue
		}
		var got []string
		for _, f := range forms {
			got = append(got, f.String())
		}
		if s := strings.Join(got, " "); s != tt.want {
			t.Errorf("Read(%q) = %s, want %s", tt.src, s, tt.want)
		}
	}
}

func TestReadPositions(t *testing.T) {
	forms, err := Read("t.puff", "(a\n  [b 1])")
	if err != nil {
		t.Fatal(err)
	}
	list := forms[0]
	vec := list.Items[1]
	for _, tt := range []struct {
		f    *Form
		want string
	}{
		{list, "t.puff:1:1"},
		{list.Items[0], "t.puff:1:2"},
		{vec, "t.puff:2:3"},
		{vec.Items[0], "t.puff:2:4"},
		{vec.Items[1], "t.puff:2:6"},
	} {
		if got := tt.f.Pos.String(); got != tt.want {
			t.Errorf("%s at %s, want %s", tt.f, got, tt.want)
		}
	}
}

func TestReadPositionsAfterText(t *testing.T) {
	forms, err := Read("t.puff", "(a \"\"\"\n    x\n    \"\"\" b)\nc")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		f    *Form
		want string
	}{
		{forms[0].Items[1], "t.puff:1:4"},
		{forms[0].Items[2], "t.puff:3:9"},
		{forms[1], "t.puff:4:1"},
	} {
		if got := tt.f.Pos.String(); got != tt.want {
			t.Errorf("%s at %s, want %s", tt.f, got, tt.want)
		}
	}
}

func TestReadErrors(t *testing.T) {
	tests := []struct {
		src  string
		want string
	}{
		{"(a b", "t.puff:1:1: no closing ')'"},
		{")", "t.puff:1:1: unexpected ')'"},
		{"(a ]", "t.puff:1:4: unexpected ']'"},
		{`"abc`, "t.puff:1:1: unclosed string"},
		{`"a\qb"`, "t.puff:1:1: bad escape in string"},
		{"#x", "t.puff:1:1: unknown #x: booleans are #t and #f"},
		{"9223372036854775808", "t.puff:1:1: int 9223372036854775808 out of range"},
		{"1e999", "t.puff:1:1: float 1e999 out of range"},
		{"106752d", "t.puff:1:1: duration 106752d out of range"},
		{"1abc", "t.puff:1:1: bad number 1abc"},
		{"1.5s", "t.puff:1:1: bad number 1.5s"},
		{"2026-10-27T10:00:00+02:00", "t.puff:1:1: time 2026-10-27T10:00:00+02:00: write it in UTC, ending in Z"},
		{"2026-13-01T00:00:00Z", "t.puff:1:1: bad time 2026-13-01T00:00:00Z"},
		{"3000-01-01T00:00:00Z", "t.puff:1:1: time 3000-01-01T00:00:00Z out of range"},
		{"2026-10-27", "t.puff:1:1: bad number 2026-10-27"},
		{"{2026-10-27T10:00:00Z 1}", "t.puff:1:2: map key 2026-10-27T10:00:00Z: must be a bool, int, string or keyword"},
		{"{:a 1 :a 2}", "t.puff:1:7: duplicate map key :a"},
		{"{:a}", "t.puff:1:1: map literal needs a value for every key"},
		{"{1.5 2}", "t.puff:1:2: map key 1.5: must be a bool, int, string or keyword"},
		{"a@b", "t.puff:1:1: bad name a@b"},
		{":", "t.puff:1:1: bad keyword :"},
		{"(a\n  b\n  @x)", "t.puff:3:3: bad name @x"},
		{"; comment\n  \"multi\nline", "t.puff:2:3: unclosed string"},
		{"\"\"\" a\n\"\"\"", `t.puff:1:1: a """ string starts on the line after its """`},
		{"(x\n  \"\"\"\n    a\n   b\n    \"\"\")", `t.puff:4:1: this line is indented less than the closing """ of the string at t.puff:2:3`},
		{"\"\"\"\n  a \"\"\"", `t.puff:1:1: unclosed """ string`},
	}
	for _, tt := range tests {
		_, err := Read("t.puff", tt.src)
		if err == nil || err.Error() != tt.want {
			t.Errorf("Read(%q) error = %v, want %s", tt.src, err, tt.want)
		}
	}
}

func TestReadValue(t *testing.T) {
	got, err := ReadValue(`(ok [1 {:a "x"}])`)
	if err != nil {
		t.Fatal(err)
	}
	want := NewTag("ok", NewVector(Int(1), mustMap(t, Keyword("a"), String("x"))))
	if !Equal(got, want) {
		t.Errorf("ReadValue = %s, want %s", Print(got), Print(want))
	}
}

func TestReadValueErrors(t *testing.T) {
	tests := []struct {
		src  string
		want string
	}{
		{"foo", "1:1: name foo in data"},
		{"[(ok x)]", "1:6: name x in data"},
		{"()", "1:1: a tag needs a name: (name args...)"},
		{"(1 2)", "1:1: a tag needs a name: (name args...)"},
		{"{[1] 2}", "1:1: map key [1]: must be a bool, int, string or keyword"},
		{"1 2", "1:3: want one value, got 2"},
		{"", "1:1: want one value, got 0"},
	}
	for _, tt := range tests {
		_, err := ReadValue(tt.src)
		if err == nil || err.Error() != tt.want {
			t.Errorf("ReadValue(%q) error = %v, want %s", tt.src, err, tt.want)
		}
	}
}

// Reading a printed value gives it back, so values can travel as text.
func TestPrintReadRoundTrip(t *testing.T) {
	values := []Value{
		Bool(true),
		Int(0), Int(math.MinInt64), Int(math.MaxInt64),
		Float(0.1), Float(1), Float(1e-7), Float(1e21), Float(math.Copysign(0, -1)),
		Float(math.MaxFloat64), Float(math.SmallestNonzeroFloat64),
		String(""), String("tab\t quote\" backslash\\ nul\x00 é 🙂 bad\xff\nline"),
		Keyword("gh/prs"),
		Duration(0), Duration(1), Duration(36 * time.Hour), Duration(-90 * time.Second),
		Duration(math.MinInt64), Duration(math.MaxInt64),
		Time{0}, Time{1}, Time{math.MinInt64}, Time{math.MaxInt64}, NewTime(time.Date(2026, 10, 27, 10, 0, 0, 5e8, time.UTC)),
		NewVector(),
		NewVector(NewTag("some", NewVector()), NewTag("none")),
		NewTag("err", mustMap(t, Keyword("kind"), Keyword("timeout"), Keyword("message"), String("slow"))),
		mustMap(t, Keyword("k"), Int(1), String("s"), Int(2), Int(3), Int(3), Bool(false), mustMap(t)),
	}
	for _, v := range values {
		s := Print(v)
		got, err := ReadValue(s)
		if err != nil {
			t.Errorf("ReadValue(%s): %v", s, err)
			continue
		}
		if !Equal(got, v) || Print(got) != s {
			t.Errorf("ReadValue(%s) = %s", s, Print(got))
		}
	}
}
