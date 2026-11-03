package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// listServices prints a line per service file, what needs you first: its
// name, one word of state and at most one note, as
//
//	procs      failed    procs.puff:12:4: unknown function str/join
//
//	assistant  waiting   2 answers in your inbox
//	gh         ok
//
// with a blank line under those that are failed, faulting or over budget.
func listServices(dir string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval("(service/list)", "")
	if err == nil && resp.Error != "" {
		err = fmt.Errorf("%s", resp.Error)
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	v, _ := puff.ReadValue(resp.Value)
	all, ok := okValue(v).(puff.Vector)
	if !ok {
		fmt.Fprintf(stderr, "whiff: want the services, got %s\n", resp.Value)
		return 1
	}
	type line struct{ name, state, note string }
	var lines []line
	nameW, stateW := 0, 0
	for i := range all.Len() {
		m, _ := all.At(i).(puff.Map)
		get := func(k string) string {
			v, _ := m.Get(puff.Keyword(k))
			switch v := v.(type) {
			case puff.Keyword:
				return string(v)
			case puff.String:
				return string(v)
			}
			return ""
		}
		l := line{get("service"), strings.ReplaceAll(get("status"), "-", " "), get("note")}
		nameW, stateW = max(nameW, len(l.name)), max(stateW, len(l.state))
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		fmt.Fprintln(stdout, "no services: add a file to ~/.whiff/services")
		return 0
	}
	trouble := func(state string) bool { return state == "failed" || state == "faulting" || state == "over budget" }
	for i, l := range lines {
		if i > 0 && trouble(lines[i-1].state) && !trouble(l.state) {
			fmt.Fprintln(stdout)
		}
		text := fmt.Sprintf("%-*s  %-*s  %s", nameW, l.name, stateW, l.state, l.note)
		fmt.Fprintln(stdout, strings.TrimRight(text, " "))
	}
	return 0
}
