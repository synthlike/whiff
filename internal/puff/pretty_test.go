package puff

import "testing"

func TestPrintPretty(t *testing.T) {
	read := func(s string) Value {
		t.Helper()
		v, err := ReadValue(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		in    string
		col   int
		width int
		want  string
	}{
		{`{:a 1 :b [2 3]}`, 0, 80, `{:a 1 :b [2 3]}`},
		{`{:a 1 :b [2 3]}`, 0, 10, "{:a 1\n :b [2 3]}"},
		{`{:a 1 :b [2 3]}`, 0, 8, "{:a 1\n :b [2\n     3]}"},
		{`(ok {:a 1 :b 2})`, 0, 10, "(ok {:a 1\n     :b 2})"},
		{`(pair "x" "y")`, 0, 10, "(pair \"x\"\n  \"y\")"},
		{`[1 2]`, 3, 6, "[1\n    2]"},
		{`["go" "test" "-run" "X" "./..."]`, 0, 16, "[\"go\" \"test\"\n \"-run\" \"X\"\n \"./...\"]"},
		{`[[1 2] 3]`, 0, 6, "[[1 2]\n 3]"},
		{`"too long for any line"`, 0, 5, `"too long for any line"`},
		{`[]`, 0, 1, `[]`},
	} {
		got := PrintPretty(read(c.in), c.col, c.width)
		if got != c.want {
			t.Errorf("PrintPretty(%s, %d, %d) =\n%s\nwant\n%s", c.in, c.col, c.width, got, c.want)
		}
		if c.col == 0 {
			if !Equal(read(got), read(c.in)) {
				t.Errorf("%q reads back as another value", got)
			}
			if f, err := Format("", got+"\n"); err != nil || f != got+"\n" {
				t.Errorf("puff -f changes %q to %q %v", got, f, err)
			}
		}
	}
}
