package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// viewText is v as the CLI shows a view, and true; false if v isn't one:
//
//	items      a line each, label, detail and action apart by tabs, for fzf;
//	           an item with :actions has a line for each, with its label
//	           before the action
//	text       as it is
//	locations  file:line:col: note, a line each, as compilers print them
//	board      a column each, "# title" and its items' lines, a blank line apart
func viewText(v puff.Value) (string, bool) {
	// A view's opts, its second argument, are for clients that refetch it.
	t, ok := v.(puff.Tag)
	if !ok || t.Len() < 1 || t.Len() > 2 {
		return "", false
	}
	str := func(m puff.Value, key string) string {
		s, _ := field(m, key).(puff.String)
		return oneLine(string(s))
	}
	var b strings.Builder
	items := func(vs puff.Vector) {
		for i := range vs.Len() {
			it := vs.At(i)
			line := str(it, "label")
			action := field(it, "action")
			actions, _ := field(it, "actions").(puff.Vector)
			if d := str(it, "detail"); d != "" || action != nil || actions.Len() > 0 {
				line += "\t" + d
			}
			if action != nil || actions.Len() == 0 {
				if action != nil {
					b.WriteString(line + "\t" + puff.Print(action) + "\n")
				} else {
					b.WriteString(line + "\n")
				}
			}
			for j := range actions.Len() {
				a := actions.At(j)
				b.WriteString(line + "\t" + str(a, "label") + "\t" + puff.Print(field(a, "action")) + "\n")
			}
		}
	}
	switch vs, _ := t.At(0).(puff.Vector); {
	case t.Name() == "text":
		s, ok := t.At(0).(puff.String)
		if !ok {
			return "", false
		}
		b.WriteString(string(s))
		if !strings.HasSuffix(string(s), "\n") {
			b.WriteByte('\n')
		}
	case t.Name() == "items":
		items(vs)
	case t.Name() == "board":
		for i := range vs.Len() {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("# " + str(vs.At(i), "title") + "\n")
			col, _ := field(vs.At(i), "items").(puff.Vector)
			items(col)
		}
	case t.Name() == "locations":
		for i := range vs.Len() {
			l := vs.At(i)
			line := str(l, "file")
			for _, key := range []string{"line", "col"} {
				if n, ok := field(l, key).(puff.Int); ok {
					line += ":" + puff.Print(n)
				}
			}
			if note := str(l, "note"); note != "" {
				line += ": " + note
			}
			b.WriteString(line + "\n")
		}
	default:
		return "", false
	}
	return b.String(), true
}

// oneLine is s with its tabs and line breaks as spaces, so it keeps to
// its field of a line.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// act sends the action of each line, an item as whiff prints it, or the
// action itself: its last tab-separated field, [:address/fn args...]. It
// takes args, or else reads the lines from stdin, as fzf hands them on,
// and prints each reply as call does. A line without an action exits 1
// before anything is sent.
func act(dir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	lines := args
	if len(lines) == 0 {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				lines = append(lines, sc.Text())
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
	}
	type action struct {
		target string
		args   puff.Vector
	}
	var actions []action
	for _, line := range lines {
		v, err := puff.ReadValue(line[strings.LastIndex(line, "\t")+1:])
		vec, ok := v.(puff.Vector)
		var target puff.Keyword
		if err == nil && ok && vec.Len() > 0 {
			target, ok = vec.At(0).(puff.Keyword)
		}
		if err != nil || !ok || vec.Len() == 0 {
			fmt.Fprintf(stderr, "whiff: %q has no action, want [:address/fn args...] at its end\n", line)
			return 1
		}
		rest := make([]puff.Value, vec.Len()-1)
		for i := range rest {
			rest[i] = vec.At(i + 1)
		}
		actions = append(actions, action{string(target), puff.NewVector(rest...)})
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	code := 0
	for _, a := range actions {
		resp, err := c.Message("call", a.target, puff.Print(a.args), wd)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			code = 1
			continue
		}
		if result("call", resp, stdout, stderr) != 0 {
			code = 1
		}
	}
	return code
}
