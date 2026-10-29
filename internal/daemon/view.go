package daemon

import (
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
)

// Views are plain puff values a service returns for a client to show:
// (items [...]), each item an action to pick; (text "..."); (locations
// [...]), places in files; and (board [...]), columns of items. The view/
// functions build them and check their shape, so every client can rely on
// it. Each takes opts too, {:changed topic}, the topic published when the
// view would answer otherwise, or a vector of them, so a client showing it
// fetches it again; they follow the view as its second argument.

// itemFields are an item's fields, in items and in a board's columns.
var itemFields = map[string]check{"label": isString, "detail": isString, "action": isAction, "actions": isActions}

// optsDoc is how each view/ function says what its opts are.
const optsDoc = " opts, {:changed :address/topic}, names the topic published when it changes, or a vector of them."

func viewBuiltins() []*puff.Builtin {
	return []*puff.Builtin{
		{Name: "view/items", Args: "items [opts]", Mark: puff.MarkRead,
			Doc: "(items items): a view of items to pick from, each {:label \"...\"} with an optional :detail string and :actions, what can be done with it, each {:label \"Move to Done\" :action [:address/fn args...]}, the message picking it sends, and an optional :ask, a question whose answer, text, a client adds as its last argument, \"\" if you give none; :action alone is one action without a label." + optsDoc,
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				return withOpts(view("items", args[0], itemFields, "label"))(args)
			}},
		{Name: "view/text", Args: "text [opts]", Mark: puff.MarkRead,
			Doc: "(text text): a view of text, shown as it is." + optsDoc,
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if _, ok := args[0].(puff.String); !ok {
					return nil, fmt.Errorf("want a string, got %s", puff.Print(args[0]))
				}
				return withOpts(puff.NewTag("text", args[0]), nil)(args)
			}},
		{Name: "view/locations", Args: "locations [opts]", Mark: puff.MarkRead,
			Doc: "(locations locations): a view of places in files, each {:file \"...\"} with an optional :line and :col, counting from 1, and :note string." + optsDoc,
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				return withOpts(view("locations", args[0], map[string]check{"file": isString, "line": isPositive, "col": isPositive, "note": isString}, "file"))(args)
			}},
		{Name: "view/board", Args: "columns [opts]", Mark: puff.MarkRead,
			Doc: "(board columns): a view of items in columns, each {:title \"...\" :items [...]}, its items as view/items has them." + optsDoc,
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				b, err := view("board", args[0], map[string]check{"title": isString}, "title", "items")
				if err != nil {
					return nil, err
				}
				cols := args[0].(puff.Vector)
				for i := range cols.Len() {
					col := cols.At(i).(puff.Map)
					items, _ := col.Get(puff.Keyword("items"))
					if _, err := view("items", items, itemFields, "label"); err != nil {
						title, _ := col.Get(puff.Keyword("title"))
						return nil, fmt.Errorf("column %s: %w", puff.Print(title), err)
					}
				}
				return withOpts(b, nil)(args)
			}},
	}
}

// withOpts is a function of a view/ function's args that returns v with
// their opts, if they have any, once they pass; or err, if v didn't build.
func withOpts(v puff.Value, err error) func(args []puff.Value) (puff.Value, error) {
	return func(args []puff.Value) (puff.Value, error) {
		if err != nil || len(args) < 2 {
			return v, err
		}
		opts, ok := args[1].(puff.Map)
		if !ok {
			return nil, fmt.Errorf("want opts as a map, {:changed :address/topic}, got %s", puff.Print(args[1]))
		}
		for i := range opts.Len() {
			e := opts.Entry(i)
			if !puff.Equal(e.Key, puff.Keyword("changed")) {
				return nil, fmt.Errorf("opts have no %s: want {:changed :address/topic}", puff.Print(e.Key))
			}
			if !isTopics(e.Val) {
				return nil, fmt.Errorf("want :changed as a topic, like :plan/changed, or a vector of them, got %s", puff.Print(e.Val))
			}
		}
		t := v.(puff.Tag)
		return puff.NewTag(t.Name(), t.At(0), opts), nil
	}
}

// isTopics says whether v is a topic or a non-empty vector of them.
func isTopics(v puff.Value) bool {
	if _, ok := v.(puff.Keyword); ok {
		return true
	}
	vec, ok := v.(puff.Vector)
	for i := range vec.Len() {
		_, isKw := vec.At(i).(puff.Keyword)
		ok = ok && isKw
	}
	return ok && vec.Len() > 0
}

// check says what a view's field wants, or "" if v will do.
type check func(v puff.Value) string

func isString(v puff.Value) string {
	if _, ok := v.(puff.String); !ok {
		return "a string"
	}
	return ""
}

func isPositive(v puff.Value) string {
	if n, ok := v.(puff.Int); !ok || n < 1 {
		return "an int from 1"
	}
	return ""
}

func isAction(v puff.Value) string {
	vec, ok := v.(puff.Vector)
	if !ok || vec.Len() == 0 {
		return "a message like [:todo/done 12]"
	}
	if _, ok := vec.At(0).(puff.Keyword); !ok {
		return "a message like [:todo/done 12]"
	}
	return ""
}

func isActions(v puff.Value) string {
	if _, err := view("actions", v, map[string]check{"label": isString, "action": isAction, "ask": isString}, "label", "action"); err != nil {
		return fmt.Sprintf("a vector of {:label \"...\" :action [:address/fn args...]} (%v)", err)
	}
	return ""
}

// view is (name vs), once each of vs is a map whose fields pass fields and
// that has the fields required.
func view(name string, vs puff.Value, fields map[string]check, required ...string) (puff.Value, error) {
	vec, ok := vs.(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want a vector of maps, got %s", puff.Print(vs))
	}
	for i := range vec.Len() {
		m, ok := vec.At(i).(puff.Map)
		if !ok {
			return nil, fmt.Errorf("want a map, got %s", puff.Print(vec.At(i)))
		}
		for _, key := range required {
			if _, ok := m.Get(puff.Keyword(key)); !ok {
				return nil, fmt.Errorf("%s has no :%s", puff.Print(m), key)
			}
		}
		for key, want := range fields {
			if v, ok := m.Get(puff.Keyword(key)); ok {
				if w := want(v); w != "" {
					return nil, fmt.Errorf("want :%s as %s, got %s", key, w, puff.Print(v))
				}
			}
		}
	}
	return puff.NewTag(name, vec), nil
}
