package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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

// showService prints one service in full, as service/status has it:
//
//	digest  agent  idle
//	  file    ~/.whiff/services/digest.puff
//	  turns   2 in 24h, $0.42 of $5.00 a day; the last 2h ago
//	  timers  wake in 22h
//
// with a line only for what there is to say.
func showService(dir, name string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval("(service/status "+puff.Print(puff.Keyword(strings.TrimPrefix(name, ":")))+")", "")
	if err == nil && resp.Error != "" {
		err = fmt.Errorf("%s", resp.Error)
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	v, _ := puff.ReadValue(resp.Value)
	if t, ok := v.(puff.Tag); ok && t.Name() == "err" {
		fmt.Fprintf(stderr, "whiff: %s", errText(t.At(0)))
		return 1
	}
	m, ok := okValue(v).(puff.Map)
	if !ok {
		fmt.Fprintf(stderr, "whiff: want a service, got %s\n", resp.Value)
		return 1
	}
	now := time.Now()
	get := func(m puff.Map, k string) puff.Value { v, _ := m.Get(puff.Keyword(k)); return v }
	text := func(m puff.Map, k string) string {
		switch v := get(m, k).(type) {
		case puff.Keyword:
			return string(v)
		case puff.String:
			return string(v)
		}
		return ""
	}
	ago := func(v puff.Value) string {
		if t, ok := v.(puff.Time); ok {
			return roughly(now.Sub(t.Go())) + " ago"
		}
		return ""
	}
	head := []string{text(m, "service")}
	if get(m, "agent") == puff.Bool(true) {
		head = append(head, "agent")
	}
	head = append(head, strings.ReplaceAll(text(m, "status"), "-", " "))
	if note := text(m, "note"); note != "" {
		head = append(head, note)
	}
	fmt.Fprintln(stdout, strings.Join(head, "  "))
	line := func(label, s string) {
		if s != "" {
			fmt.Fprintf(stdout, "  %-7s %s\n", label, s)
		}
	}
	file := text(m, "file")
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(file, home+"/") {
		file = "~" + file[len(home):]
	}
	line("file", file)
	line("error", text(m, "load-error"))
	if f, ok := get(m, "fault").(puff.Map); ok {
		line("faults", fmt.Sprintf("%s today; the last %s: %s", puff.Print(get(m, "faults")), ago(get(f, "at")), text(f, "message")))
	}
	if l, ok := get(m, "last").(puff.Map); ok {
		line("last", fmt.Sprintf("%s %s", text(l, "fn"), ago(get(l, "at"))))
	}
	if turns, ok := get(m, "turns").(puff.Int); ok {
		s := fmt.Sprintf("%d in 24h, $%.2f of $%.2f a day", turns, float64(get(m, "spent").(puff.Float)), float64(get(m, "day").(puff.Float)))
		if t := ago(get(m, "last-turn")); t != "" {
			s += "; the last " + t
		}
		line("turns", s)
	}
	if e, ok := get(m, "every").(puff.Duration); ok {
		line("every", puff.Print(e))
	}
	if n, ok := get(m, "inbox").(puff.Int); ok && n > 0 {
		line("inbox", fmt.Sprintf("%d waiting", n))
	}
	if ts, ok := get(m, "timers").(puff.Vector); ok && ts.Len() > 0 {
		var due []string
		for i := range ts.Len() {
			t, _ := ts.At(i).(puff.Map)
			at, _ := get(t, "due").(puff.Time)
			due = append(due, text(t, "fn")+" in "+roughly(at.Go().Sub(now)))
		}
		line("timers", strings.Join(due, ", "))
	}
	return 0
}

// roughly is d for people, in its largest unit: 40s, 12m, 3h, 2d.
func roughly(d time.Duration) string {
	switch d = max(d, 0); {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
