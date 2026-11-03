package main

import (
	"embed"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// Views as HTML: sniff draws a view a service answers with here, with
// html/template, rather than in the page, so what it draws is tested with
// go test and escaped as it is drawn.

//go:embed tmpl
var tmplFS embed.FS

var pages = template.Must(template.ParseFS(tmplFS, "tmpl/*.html"))

// shown is a reply as sniff draws it: the view it holds, if it holds one
// sniff knows, and the reply folded as puff.
type shown struct {
	View  *view
	Value node
	Next  []act // what an err reply offers instead, its :actions
}

// view is a view as a service answers with it, (ok view): (items [...]),
// (board [...]), (text "...") or (locations [...]), each with its opts
// after it if it has any.
type view struct {
	Kind    string // items, board, text or locations
	Text    string
	Items   []item
	Columns []column
	Places  []place
	Topics  []string // what its opts name to follow, {:changed topic}
}

type column struct {
	Title string
	Items []item
}

type item struct {
	Label   prose
	Detail  string
	URL     string // its page, http or https only
	Actions []act
}

// act is an item's action as the page offers it: a link to the page of a
// :read function's answer, or a form that sends it as you, asking first if
// it has :ask, and goes back to the page it is on.
type act struct {
	Label    string // what it says: its label, or its message
	Labelled bool
	Sends    string // its message, printed
	Target   string // address/fn
	Link     string // a :read function's page; "" for a form
	Write    bool   // to a :write function, so a labelled one shows its target: a label can lie
	Ask      string
	Back     string // the page it goes back to
}

// place is a location of a view, as file:line:col, and its note.
type place struct {
	At, Note string
}

// show is printed, a reply, as shown: marks is each function's mark by
// target, "read" or "write", deciding which actions are links; back is the
// page an action's form goes back to, or "" for none: a reply in history,
// whose actions may no longer hold, offers only its links.
func show(printed string, marks map[string]string, back string) shown {
	v, err := puff.ReadValue(printed)
	if err != nil {
		return shown{Value: node{Kind: "lit", Text: printed}}
	}
	return shown{View: viewOf(v, marks, back), Value: fold(v, 0), Next: nextOf(v, marks, back)}
}

// nextOf is what reply offers instead if it is an err with :actions, as
// actions going back to back; nil for any other reply.
func nextOf(reply puff.Value, marks map[string]string, back string) []act {
	t, ok := reply.(puff.Tag)
	if !ok || t.Name() != "err" || t.Len() != 1 {
		return nil
	}
	acts, _ := get(t.At(0), "actions").(puff.Vector)
	return actsOf(acts, marks, back)
}

// viewOf is the view in reply, or nil if it holds none, or one of a shape
// sniff doesn't know.
func viewOf(reply puff.Value, marks map[string]string, back string) *view {
	r, ok := reply.(puff.Tag)
	if !ok || r.Name() != "ok" || r.Len() != 1 {
		return nil
	}
	t, ok := r.At(0).(puff.Tag)
	if !ok || t.Len() < 1 || t.Len() > 2 {
		return nil
	}
	v := &view{Kind: t.Name()}
	if t.Len() == 2 {
		v.Topics = topicsOf(t.At(1))
	}
	arg := t.At(0)
	switch v.Kind {
	case "text":
		s, ok := arg.(puff.String)
		if !ok {
			return nil
		}
		v.Text = string(s)
	case "items":
		if v.Items, ok = itemsOf(arg, marks, back); !ok {
			return nil
		}
	case "board":
		cols, ok := arg.(puff.Vector)
		if !ok {
			return nil
		}
		for i := range cols.Len() {
			title, ok := strOf(cols.At(i), "title")
			its, ok2 := itemsOf(get(cols.At(i), "items"), marks, back)
			if !ok || !ok2 {
				return nil
			}
			v.Columns = append(v.Columns, column{Title: title, Items: its})
		}
	case "locations":
		ps, ok := arg.(puff.Vector)
		if !ok {
			return nil
		}
		for i := range ps.Len() {
			at, ok := strOf(ps.At(i), "file")
			if !ok {
				return nil
			}
			for _, key := range []string{"line", "col"} {
				if n, ok := get(ps.At(i), key).(puff.Int); ok {
					at += ":" + puff.Print(n)
				}
			}
			note, _ := strOf(ps.At(i), "note")
			v.Places = append(v.Places, place{At: at, Note: note})
		}
	default:
		return nil
	}
	return v
}

// topicsOf is what opts names to follow, {:changed :plan/changed} or a
// vector of them.
func topicsOf(opts puff.Value) []string {
	var ts []string
	switch t := get(opts, "changed").(type) {
	case puff.Keyword:
		ts = append(ts, string(t))
	case puff.Vector:
		for i := range t.Len() {
			if k, ok := t.At(i).(puff.Keyword); ok {
				ts = append(ts, string(k))
			}
		}
	}
	return ts
}

// itemsOf is v, a vector of items, each a map with a :label string, with
// its :detail, its :url if it is http or https, and what can be done with
// it: its :actions, and its :action, labelled with its message; false if v
// isn't such a vector.
func itemsOf(v puff.Value, marks map[string]string, back string) ([]item, bool) {
	vec, ok := v.(puff.Vector)
	if !ok {
		return nil, false
	}
	its := []item{}
	for i := range vec.Len() {
		m := vec.At(i)
		label, ok := strOf(m, "label")
		if !ok {
			return nil, false
		}
		it := item{Label: proseOf(label)}
		it.Detail, _ = strOf(m, "detail")
		// view/items checks it too, but a service can build (items ...) itself.
		if u, _ := strOf(m, "url"); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			it.URL = u
		}
		if msg, ok := get(m, "action").(puff.Vector); ok {
			if a, ok := actOf("", msg, "", marks, back); ok {
				it.Actions = append(it.Actions, a)
			}
		}
		acts, _ := get(m, "actions").(puff.Vector)
		it.Actions = append(it.Actions, actsOf(acts, marks, back)...)
		its = append(its, it)
	}
	return its, true
}

// actsOf is acts, [{:label :action [...] :ask}...], as actions, leaving
// out any whose :action isn't a message.
func actsOf(acts puff.Vector, marks map[string]string, back string) []act {
	var out []act
	for j := range acts.Len() {
		msg, _ := get(acts.At(j), "action").(puff.Vector)
		label, _ := strOf(acts.At(j), "label")
		ask, _ := strOf(acts.At(j), "ask")
		if a, ok := actOf(label, msg, ask, marks, back); ok {
			out = append(out, a)
		}
	}
	return out
}

// actOf is msg, [:address/fn args...], as an action labelled label; false
// if it isn't a message.
func actOf(label string, msg puff.Vector, ask string, marks map[string]string, back string) (act, bool) {
	if msg.Len() == 0 {
		return act{}, false
	}
	k, ok := msg.At(0).(puff.Keyword)
	if !ok {
		return act{}, false
	}
	a := act{Label: label, Labelled: label != "", Sends: puff.Print(msg), Target: string(k), Ask: ask, Back: back}
	if a.Label == "" {
		a.Label = a.Sends
	}
	switch marks[a.Target] {
	case "read":
		args := make([]puff.Value, msg.Len()-1)
		for i := range args {
			args[i] = msg.At(i + 1)
		}
		a.Link = pageOf(a.Target, puff.NewVector(args...))
	case "write":
		a.Write = true
	}
	return a, a.Link != "" || back != ""
}

// pageOf is the page that shows what target answers to args as a view:
// /view/pipeline/show?args=[2].
func pageOf(target string, args puff.Vector) string {
	p := (&url.URL{Path: "/view/" + target}).EscapedPath()
	if args.Len() > 0 {
		p += "?args=" + url.QueryEscape(puff.Print(args))
	}
	return p
}

// get is map m's value at the keyword key, or nil.
func get(m puff.Value, key string) puff.Value {
	if mm, ok := m.(puff.Map); ok {
		v, _ := mm.Get(puff.Keyword(key))
		return v
	}
	return nil
}

// strOf is map m's string at key, and whether it has one.
func strOf(m puff.Value, key string) (string, bool) {
	s, ok := get(m, key).(puff.String)
	return string(s), ok
}

// prose is text, a report or a task, as Claude and people write it: a line
// as it is, Plain; more than that as blocks: paragraphs, lists whose lines
// start "1. " or "- ", and ```code blocks```, with `code` in them.
type prose struct {
	Plain  bool
	Text   string
	Blocks []block
}

// block is a paragraph, p, of one line of spans; a list, ol or ul, of a
// line each, numbered from Start; or code, pre.
type block struct {
	Tag   string
	Start int
	Lines [][]span
	Code  string
}

// span is text, or code if Code.
type span struct {
	Text string
	Code bool
}

var (
	fence    = regexp.MustCompile("(?m)^```[^\n]*\n?")
	blank    = regexp.MustCompile(`\n\s*\n`)
	numbered = regexp.MustCompile(`^\s*(\d+)[.)]\s`)
	bulleted = regexp.MustCompile(`^\s*[-*]\s`)
	marker   = regexp.MustCompile(`^\s*(\d+[.)]|[-*])\s+`)
)

func proseOf(text string) prose {
	if !strings.ContainsAny(text, "\n`") {
		return prose{Plain: true, Text: text}
	}
	var p prose
	// Code fences split text: every other part is a block of code.
	for i, part := range fence.Split(text, -1) {
		if i%2 == 1 {
			p.Blocks = append(p.Blocks, block{Tag: "pre", Code: strings.TrimSuffix(part, "\n")})
			continue
		}
		for _, para := range blank.Split(part, -1) {
			var lines []string
			for _, l := range strings.Split(para, "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
			if len(lines) == 0 {
				continue
			}
			list := ""
			switch {
			case all(lines, numbered.MatchString):
				list = "ol"
			case all(lines, bulleted.MatchString):
				list = "ul"
			}
			if list == "" {
				p.Blocks = append(p.Blocks, block{Tag: "p", Lines: [][]span{spans(strings.Join(lines, "\n"))}})
				continue
			}
			b := block{Tag: list}
			if list == "ol" {
				b.Start, _ = strconv.Atoi(numbered.FindStringSubmatch(lines[0])[1])
			}
			for _, l := range lines {
				b.Lines = append(b.Lines, spans(marker.ReplaceAllString(l, "")))
			}
			p.Blocks = append(p.Blocks, b)
		}
	}
	return p
}

func all(lines []string, f func(string) bool) bool {
	for _, l := range lines {
		if !f(l) {
			return false
		}
	}
	return true
}

// spans is text with `code` in it as code.
func spans(text string) []span {
	var ss []span
	for i, s := range strings.Split(text, "`") {
		if s != "" {
			ss = append(ss, span{Text: s, Code: i%2 == 1})
		}
	}
	return ss
}

// node is a puff value as the page folds it: short ones on a line, longer
// ones as a tree, open at its first two levels.
type node struct {
	Kind  string // map, vec, tag, kw, str or lit
	Text  string // the value printed
	Name  string // a tag's
	Kids  []node // a vector's or tag's items
	Pairs []pair // a map's
	Fold  bool
	Open  bool
	Short string // a long string's start, what it shows folded
	Raw   string // a long string as it is, what it shows open: a report reads as text
}

// A string longer than longString folds, showing its first shortString
// runes.
const (
	longString  = 160
	shortString = 100
)

type pair struct {
	Key, Val node
}

// Count is how many items or pairs n has, as its folded line says.
func (n node) Count() int { return len(n.Kids) + len(n.Pairs) }

func fold(v puff.Value, depth int) node {
	n := node{Text: puff.Print(v), Open: depth < 2}
	switch v := v.(type) {
	case puff.Map:
		n.Kind = "map"
		for i := range v.Len() {
			e := v.Entry(i)
			n.Pairs = append(n.Pairs, pair{fold(e.Key, depth+1), fold(e.Val, depth+1)})
		}
	case puff.Vector:
		n.Kind = "vec"
		for i := range v.Len() {
			n.Kids = append(n.Kids, fold(v.At(i), depth+1))
		}
	case puff.Tag:
		n.Kind, n.Name = "tag", v.Name()
		for i := range v.Len() {
			n.Kids = append(n.Kids, fold(v.At(i), depth+1))
		}
	case puff.Keyword:
		n.Kind = "kw"
	case puff.String:
		n.Kind = "str"
		if r := []rune(n.Text); len(r) > longString {
			n.Fold, n.Open, n.Short, n.Raw = true, false, string(r[:shortString])+"…", string(v)
		}
		return n
	default:
		n.Kind = "lit"
	}
	n.Fold = len(n.Text) > 64 && (n.Kind == "map" || n.Kind == "vec" || n.Kind == "tag")
	return n
}
