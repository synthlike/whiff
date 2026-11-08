// Package tools describes whiff functions as tools for a model, and
// reads its calls and their answers: what whiff mcp offers Claude, and
// what huff offers the models it runs. Each client keeps its own
// connection and calls the functions itself; this package only says what
// to offer and how to read a call and its answer.
package tools

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// Mode is an agent session's mode, as (session/mode) gives it: what it
// lets through without review.
type Mode struct {
	Printed string
	Free    bool
	Trust   map[string]bool // trusted: the services whose messages go through
	Until   string          // when it runs out, or ""
}

// ModeOf is the mode v, (session/mode)'s value.
func ModeOf(v puff.Value) Mode {
	v = puff.OkValue(v)
	kind, _ := puff.Field(v, "mode").(puff.Keyword)
	m := Mode{Printed: puff.Print(v), Free: kind == "free", Trust: map[string]bool{}}
	services, _ := puff.Field(v, "services").(puff.Vector)
	for i := range services.Len() {
		if k, ok := services.At(i).(puff.Keyword); ok {
			m.Trust[string(k)] = true
		}
	}
	if t := puff.Field(v, "until"); t != nil {
		m.Until = puff.Print(t)
	}
	return m
}

// Lets is whether m lets messages to addr through without review.
func (m Mode) Lets(addr string) bool { return m.Free || m.Trust[addr] }

// Fn is a service function as a tool.
type Fn struct {
	Name        string // address_fn, as Name gives it
	Title       string // :address/fn
	Description string
	Schema      map[string]any // its arguments, a JSON Schema object
	ReadOnly    bool
	Target      string // address/fn
	Params      []string
	Waits       bool // a :write that waits for the user's review
}

// Describe is every function of the addresses, as eval, the session's
// own evaluation, gives them, as a tool: but self's and those only you
// may use, :yours, and, given can, only those it names; described as
// mode treats them.
func Describe(eval func(src string) (puff.Value, error), self string, can map[string]bool, mode Mode) ([]Fn, error) {
	var addrs puff.Vector
	if can != nil {
		// Only the addresses of its :can functions, which it may describe.
		seen := map[string]bool{}
		var vals []puff.Value
		for _, fn := range slices.Sorted(maps.Keys(can)) {
			addr := fn[:strings.LastIndex(fn, "/")]
			if !seen[addr] {
				seen[addr] = true
				m, _ := puff.NewMap(puff.Key("address", puff.Keyword(addr)))
				vals = append(vals, m)
			}
		}
		addrs = puff.NewVector(vals...)
	} else {
		v, err := eval("(address/list)")
		if err != nil {
			return nil, err
		}
		addrs, _ = puff.OkValue(v).(puff.Vector)
	}
	var fns []Fn
	taken := map[string]bool{}
	for i := range addrs.Len() {
		addr, _ := puff.Field(addrs.At(i), "address").(puff.Keyword)
		if addr == "" || string(addr) == self {
			continue
		}
		v, err := eval("(address/describe " + puff.Print(addr) + ")")
		if err != nil {
			return nil, err
		}
		described, _ := puff.OkValue(v).(puff.Vector)
		for j := range described.Len() {
			f := described.At(j)
			if yours, _ := puff.Field(f, "yours").(puff.Bool); yours {
				continue
			}
			fn, _ := puff.Field(f, "fn").(puff.Keyword)
			doc, _ := puff.Field(f, "doc").(puff.String)
			mark, _ := puff.Field(f, "mark").(puff.Keyword)
			form, _ := puff.Field(f, "form").(puff.Keyword)
			ps, _ := puff.Field(f, "params").(puff.Vector)
			t := Fn{Target: string(addr) + "/" + string(fn)}
			t.Name, t.Title = Name(t.Target), ":"+t.Target
			if can != nil && !can[t.Target] || taken[t.Name] {
				continue
			}
			taken[t.Name] = true
			props := map[string]any{}
			for k := range ps.Len() {
				p, _ := ps.At(k).(puff.Keyword)
				t.Params = append(t.Params, string(p))
				props[string(p)] = map[string]any{"type": "string", "description": "a puff value, or a JSON array or object; text that reads as neither is a string"}
			}
			how := "Answers at once."
			switch {
			case form == "ask":
				how = "An ask: it waits for a person to answer, so you get a pending id; wait gives the answer."
			case mark == "write" && mode.Lets(string(addr)):
				how = "Your session's mode lets it through without review: it runs at once."
			case mark == "write":
				how = "It waits for the user's review, so you get a pending id; wait gives the result."
				t.Waits = true
			}
			t.Description = fmt.Sprintf("%s\n\n:%s, %s. %s", doc, t.Target, puff.Print(mark), how)
			t.Schema = map[string]any{"type": "object", "properties": props, "additionalProperties": false}
			t.ReadOnly = mark == "read" && form != "ask"
			fns = append(fns, t)
		}
	}
	return fns, nil
}

// Args are a call's arguments, a JSON object of strings, as puff values
// in f's params' order, up to the first left out.
func Args(f Fn, raw json.RawMessage) (puff.Vector, error) {
	var args map[string]json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return puff.Vector{}, fmt.Errorf("want arguments as an object: %v", err)
		}
	}
	var vals []puff.Value
	for _, p := range f.Params {
		raw, ok := args[p]
		if !ok {
			break
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			// Models write numbers and objects bare: read them as JSON.
			v, jerr := puff.ParseJSON(string(raw))
			if jerr != nil {
				return puff.Vector{}, fmt.Errorf("%s: want a string holding a puff value, got %s", p, raw)
			}
			vals = append(vals, v)
			continue
		}
		vals = append(vals, Arg(s))
	}
	return puff.NewVector(vals...), nil
}

// Arg is a tool's argument s as a puff value: as it reads, or, as
// models write vectors and maps as JSON, ["go","test"], a JSON array or
// object read as one; else s itself, a string.
func Arg(s string) puff.Value {
	if v, err := puff.ReadValue(s); err == nil {
		return v
	}
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
		if v, err := puff.ParseJSON(t); err == nil {
			return v
		}
	}
	return puff.String(s)
}

// Name is address/fn as a tool's name: gh_prs, agent_2_tell; any
// character a name can't have is _.
func Name(target string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, target)
}

// Result is a printed Result as a tool's answer: the value of (ok v), a
// string as it is; a pending (err e) with its id; any other (err e) as an
// error. A value that isn't a Result is printed as it is. waiting says
// what the model does with a pending id, as "call wait with this id for
// the result".
func Result(printed, waiting string) (text string, isError bool) {
	v, err := puff.ReadValue(printed)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 || r.Name() != "ok" && r.Name() != "err" {
		return printed, false
	}
	if r.Name() == "ok" {
		if s, ok := r.At(0).(puff.String); ok {
			return string(s), false
		}
		return puff.Print(r.At(0)), false
	}
	if puff.Equal(puff.Field(r.At(0), "kind"), puff.Keyword("pending")) {
		msg, _ := puff.Field(r.At(0), "message").(puff.String)
		return fmt.Sprintf("pending %s: %s; %s", puff.Print(puff.Field(r.At(0), "id")), msg, waiting), false
	}
	return puff.Print(r.At(0)), true
}

// Offers are the actions in printed's value, as a view's items have
// them, each as a line naming the tool call that sends it, from fns by
// address/fn: what the model can do from here. An action is an offer,
// sent under the same rules as any call.
func Offers(printed string, fns map[string]Fn) []string {
	v, _ := puff.ReadValue(printed)
	var lines []string
	for _, act := range actionsIn(puff.OkValue(v)) {
		if line, ok := toolCall(act, fns); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

// actionsIn is every action in v, a map with an :action message, in order.
func actionsIn(v puff.Value) []puff.Map {
	var acts []puff.Map
	var walk func(puff.Value)
	walk = func(v puff.Value) {
		switch v := v.(type) {
		case puff.Map:
			if _, ok := puff.Field(v, "action").(puff.Vector); ok {
				acts = append(acts, v)
				return
			}
			for i := range v.Len() {
				walk(v.Entry(i).Val)
			}
		case puff.Vector:
			for i := range v.Len() {
				walk(v.At(i))
			}
		case puff.Tag:
			for i := range v.Len() {
				walk(v.At(i))
			}
		}
	}
	walk(v)
	return acts
}

// toolCall is action act as a line: its label, the tool that sends it and
// the tool's arguments, with a placeholder for the answer an :ask action
// wants; false if no tool sends it, as for a function only the user may
// use.
func toolCall(act puff.Map, fns map[string]Fn) (string, bool) {
	label, _ := puff.Field(act, "label").(puff.String)
	msg, _ := puff.Field(act, "action").(puff.Vector)
	if msg.Len() == 0 {
		return "", false
	}
	target, _ := msg.At(0).(puff.Keyword)
	f, ok := fns[string(target)]
	ask, asks := puff.Field(act, "ask").(puff.String)
	n := msg.Len() - 1
	if !ok || asks && n >= len(f.Params) || n > len(f.Params) {
		return "", false
	}
	var args []string
	for i := range n {
		args = append(args, fmt.Sprintf("%q: %q", f.Params[i], puff.Print(msg.At(i+1))))
	}
	if asks {
		args = append(args, fmt.Sprintf("%q: <your answer to %s>", f.Params[n], puff.Print(ask)))
	}
	line := fmt.Sprintf("- %s: %s {%s}", label, f.Name, strings.Join(args, ", "))
	if f.Waits {
		line += ", which waits for the user's review"
	}
	return line, true
}
