package rpc

import (
	"fmt"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// Query is a filter of history, as whiff history and sniff take it: terms
// split by spaces, each of which an entry must match, and a term with a "-"
// before it, which it must not.
//
//	to:plan        sent to :plan, any function or topic of it; to:plan/draft only that
//	from:agent/3   sent by :agent/3; from:agent by any agent session; from:client by a client
//	origin:agent   an agent's, or one a chain of an agent's led to; origin:user
//	kind:call      what it is: call, send, pub, done, eval or note
//	kind:err       what it answered: ok, err (a fault too), fault or pending
//	time:>500ms    took longer than 500ms; time:<1ms shorter
//	mark:write     to a :write function, or an agent's eval that writes; mark:read
//	gate:held      how the review gate treated it: held, refused, accepted, trusted, free or for
//	word           its target, arguments, reply or note hold word, in any case
//
// So "to:plan kind:err -gate:held" is what :plan answered with an error,
// but not what waits for review.
type Query struct {
	terms []term
}

type term struct {
	not      bool
	key, val string
	less     bool // time:<
	took     time.Duration
}

// ParseQuery reads s as a Query, or says which term it can't.
func ParseQuery(s string) (Query, error) {
	var q Query
	for _, f := range strings.Fields(s) {
		t := term{}
		if len(f) > 1 && f[0] == '-' {
			t.not, f = true, f[1:]
		}
		key, val, ok := strings.Cut(f, ":")
		if !ok || key == "" {
			t.val = strings.ToLower(f)
			q.terms = append(q.terms, t)
			continue
		}
		t.key, t.val = key, strings.TrimPrefix(val, ":")
		if t.val == "" {
			return Query{}, fmt.Errorf("%s: want something after %s:", f, key)
		}
		switch key {
		case "to", "from", "gate":
		case "origin":
			if t.val != "user" && t.val != "agent" {
				return Query{}, fmt.Errorf("%s: want origin:user or origin:agent", f)
			}
		case "kind":
			switch t.val {
			case "call", "send", "pub", "done", "eval", "note", "ok", "err", "fault", "pending":
			default:
				return Query{}, fmt.Errorf("%s: want kind: call, send, pub, done, eval or note, or ok, err, fault or pending", f)
			}
		case "mark":
			if t.val != "read" && t.val != "write" {
				return Query{}, fmt.Errorf("%s: want mark:read or mark:write", f)
			}
		case "time":
			d, err := time.ParseDuration(strings.TrimLeft(t.val, "<>"))
			if err != nil || (t.val[0] != '<' && t.val[0] != '>') {
				return Query{}, fmt.Errorf("%s: want time:>500ms or time:<2s", f)
			}
			t.less, t.took = t.val[0] == '<', d
		default:
			return Query{}, fmt.Errorf("%s: no such filter; want to:, from:, origin:, kind:, time:, mark: or gate:", f)
		}
		q.terms = append(q.terms, t)
	}
	return q, nil
}

// Empty is whether q matches every entry.
func (q Query) Empty() bool { return len(q.terms) == 0 }

// Marks is whether q needs each function's mark to match: it has a mark:
// term.
func (q Query) Marks() bool {
	for _, t := range q.terms {
		if t.key == "mark" {
			return true
		}
	}
	return false
}

// Match is whether e matches q, with marks each function's mark, "read"
// or "write", by its target, address/fn: nil will do if q has no mark:
// term.
func (q Query) Match(e Entry, marks map[string]string) bool {
	for _, t := range q.terms {
		if t.match(e, marks) == t.not {
			return false
		}
	}
	return true
}

func (t term) match(e Entry, marks map[string]string) bool {
	switch t.key {
	case "":
		for _, s := range []string{e.To, e.Args, e.Reply, e.Note} {
			if strings.Contains(strings.ToLower(s), t.val) {
				return true
			}
		}
		return false
	case "to":
		return e.To == t.val || strings.HasPrefix(e.To, t.val+"/") || strings.HasPrefix(e.To, t.val+" on :")
	case "from":
		if t.val == "client" {
			return e.From == "" && e.Kind != "note"
		}
		return e.From == t.val || strings.HasPrefix(e.From, t.val+"/")
	case "origin":
		return e.Origin == t.val
	case "kind":
		if e.Kind == t.val {
			return true
		}
		k := replyKind(e.Reply)
		return k == t.val || (t.val == "err" && k == "fault")
	case "time":
		if e.Took <= 0 {
			return false
		}
		if t.less {
			return e.Took < t.took
		}
		return e.Took > t.took
	case "mark":
		if e.Kind == "eval" {
			return t.val == "write"
		}
		return (e.Kind == "call" || e.Kind == "send") && marks[e.To] == t.val
	case "gate":
		kind, _, _ := strings.Cut(e.Gate, " ")
		return kind == t.val
	}
	return false
}

// replyKind is what reply, a printed Result, says: "ok", "err", "fault" or
// "pending"; "" for none.
func replyKind(reply string) string {
	v, err := puff.ReadValue(reply)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 {
		return ""
	}
	if r.Name() == "ok" {
		return "ok"
	}
	if m, ok := r.At(0).(puff.Map); ok {
		if k, ok := m.Get(puff.Keyword("kind")); ok && (puff.Equal(k, puff.Keyword("fault")) || puff.Equal(k, puff.Keyword("pending"))) {
			return string(k.(puff.Keyword))
		}
	}
	return "err"
}

// Marks is every function's mark, "read" or "write", by its target,
// address/fn, as address/describe says: what a Query's mark: matches, and
// whether an action to it is a link or a form.
func (c *Client) Marks() (map[string]string, error) {
	addrs, err := c.EvalOk("(address/list)")
	if err != nil {
		return nil, err
	}
	ms := map[string]string{}
	key := func(v puff.Value, k string) string {
		m, _ := v.(puff.Map)
		got, _ := m.Get(puff.Keyword(k))
		s, _ := got.(puff.Keyword)
		return string(s)
	}
	vec, _ := addrs.(puff.Vector)
	for i := range vec.Len() {
		addr := key(vec.At(i), "address")
		fns, err := c.EvalOk("(address/describe " + puff.Print(puff.Keyword(addr)) + ")")
		if err != nil {
			continue // gone since
		}
		fv, _ := fns.(puff.Vector)
		for j := range fv.Len() {
			ms[addr+"/"+key(fv.At(j), "fn")] = key(fv.At(j), "mark")
		}
	}
	return ms, nil
}

// EvalOk evaluates src, read-only code of a client's own, and returns v
// of its (ok v); anything else is an error.
func (c *Client) EvalOk(src string) (puff.Value, error) {
	v, err := c.EvalValue(src, "")
	if err != nil {
		return nil, err
	}
	if t, ok := v.(puff.Tag); ok && t.Name() == "ok" && t.Len() == 1 {
		return t.At(0), nil
	}
	return nil, fmt.Errorf("%s: %s", src, puff.Print(v))
}
