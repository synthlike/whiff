package daemon

import (
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
)

// Views are plain puff values a service returns for a client to show:
// (items [...]), each item an action to pick; (text "..."); and (locations
// [...]), places in files. The view/ functions build them and check their
// shape, so every client can rely on it.

func viewBuiltins() []*puff.Builtin {
	return []*puff.Builtin{
		{Name: "view/items", Args: "items", Mark: puff.MarkRead,
			Doc: "(items items): a view of items to pick from, each {:label \"...\"} with an optional :detail string and :action, a message as [:address/fn args...] that picking it sends.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				return view("items", args[0], map[string]check{"label": isString, "detail": isString, "action": isAction}, "label")
			}},
		{Name: "view/text", Args: "text", Mark: puff.MarkRead,
			Doc: "(text text): a view of text, shown as it is.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if _, ok := args[0].(puff.String); !ok {
					return nil, fmt.Errorf("want a string, got %s", puff.Print(args[0]))
				}
				return puff.NewTag("text", args[0]), nil
			}},
		{Name: "view/locations", Args: "locations", Mark: puff.MarkRead,
			Doc: "(locations locations): a view of places in files, each {:file \"...\"} with an optional :line and :col, counting from 1, and :note string.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				return view("locations", args[0], map[string]check{"file": isString, "line": isPositive, "col": isPositive, "note": isString}, "file")
			}},
	}
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

// view is (name vs), once each of vs is a map whose fields pass fields and
// that has the field required.
func view(name string, vs puff.Value, fields map[string]check, required string) (puff.Value, error) {
	vec, ok := vs.(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want a vector of maps, got %s", puff.Print(vs))
	}
	for i := range vec.Len() {
		m, ok := vec.At(i).(puff.Map)
		if !ok {
			return nil, fmt.Errorf("want a map, got %s", puff.Print(vec.At(i)))
		}
		if _, ok := m.Get(puff.Keyword(required)); !ok {
			return nil, fmt.Errorf("%s has no :%s", puff.Print(m), required)
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
