package main

import (
	"encoding/json"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// tree is a puff value as the page shows it, folded:
//
//	{"t": "map", "v": [[key, val], ...]}
//	{"t": "vec", "v": [item, ...]}
//	{"t": "tag", "n": "ok", "v": [arg, ...]}
//	{"t": "kw", "v": "name"}, {"t": "str", "v": "text"}
//	{"t": "lit", "v": "12"}: anything else, printed
type tree struct {
	T string `json:"t"`
	N string `json:"n,omitempty"`
	V any    `json:"v"`
}

// treeOf is printed, a printed puff value, as a tree; text that doesn't
// read is a literal.
func treeOf(printed string) *tree {
	v, err := puff.ReadValue(printed)
	if err != nil {
		return &tree{T: "lit", V: printed}
	}
	return toTree(v)
}

func toTree(v puff.Value) *tree {
	switch v := v.(type) {
	case puff.Map:
		es := make([][2]*tree, v.Len())
		for i := range v.Len() {
			e := v.Entry(i)
			es[i] = [2]*tree{toTree(e.Key), toTree(e.Val)}
		}
		return &tree{T: "map", V: es}
	case puff.Vector:
		return &tree{T: "vec", V: items(v.Len(), v.At)}
	case puff.Tag:
		return &tree{T: "tag", N: v.Name(), V: items(v.Len(), v.At)}
	case puff.Keyword:
		return &tree{T: "kw", V: string(v)}
	case puff.String:
		return &tree{T: "str", V: string(v)}
	}
	return &tree{T: "lit", V: puff.Print(v)}
}

func items(n int, at func(int) puff.Value) []*tree {
	ts := make([]*tree, n)
	for i := range n {
		ts[i] = toTree(at(i))
	}
	return ts
}

// plain is v as plain JSON, for data whose shape sniff knows (describe's):
// maps by their keys' names, keywords as their names.
func plain(v puff.Value) any {
	switch v := v.(type) {
	case puff.Map:
		m := map[string]any{}
		for i := range v.Len() {
			e := v.Entry(i)
			k, ok := e.Key.(puff.Keyword)
			if !ok {
				m[puff.Print(e.Key)] = plain(e.Val)
				continue
			}
			m[string(k)] = plain(e.Val)
		}
		return m
	case puff.Vector:
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = plain(v.At(i))
		}
		return out
	case puff.Keyword:
		return string(v)
	case puff.String:
		return string(v)
	case puff.Bool:
		return bool(v)
	case puff.Int:
		return int64(v)
	}
	return puff.Print(v)
}

// entryJSON is an entry of history as pages get it: its args and reply as
// trees, how long it took in milliseconds, and when it began also in
// microseconds since 1970, which a JavaScript number holds exactly.
func entryJSON(e rpc.Entry) json.RawMessage {
	out := struct {
		At      time.Time `json:"at"`
		Us      int64     `json:"us"`
		Kind    string    `json:"kind"`
		ID      int64     `json:"id,omitempty"`
		Cause   int64     `json:"cause,omitempty"`
		Origin  string    `json:"origin,omitempty"`
		From    string    `json:"from"`
		To      string    `json:"to"`
		Args    *tree     `json:"args,omitempty"`
		Reply   *tree     `json:"reply,omitempty"`
		Took    float64   `json:"took"`
		Gate    string    `json:"gate,omitempty"`
		Note    string    `json:"note,omitempty"`
		Project string    `json:"project,omitempty"`
	}{At: e.At, Us: e.At.UnixMicro(), Kind: e.Kind, ID: e.ID, Cause: e.Cause, Origin: e.Origin, From: e.From, To: e.To, Took: float64(e.Took) / float64(time.Millisecond), Gate: e.Gate, Note: e.Note, Project: e.Project}
	if e.Args != "" {
		out.Args = treeOf(e.Args)
	}
	if e.Reply != "" {
		out.Reply = treeOf(e.Reply)
	}
	data, _ := json.Marshal(out)
	return data
}
