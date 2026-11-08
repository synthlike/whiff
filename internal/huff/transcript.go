package huff

import (
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
)

// A transcript as puff data, readable in history and a REPL, and what a
// run takes back as its :history:
//
//	[{:role :user :text "what's on my list?"}
//	 {:role :assistant :calls [{:id "c1" :fn "todo_list" :args "{}"}]}
//	 {:role :tool :id "c1" :result "[...]"}
//	 {:role :assistant :text "Two things: ..."}]
//
// A key is there only when it says something: an assistant's :text when
// it wrote some, :calls when it made some, a tool's :error #t when the
// call failed.

// ToPuff is ms as puff data.
func ToPuff(ms []Message) puff.Vector {
	vs := make([]puff.Value, len(ms))
	for i, m := range ms {
		es := []puff.Entry{puff.Key("role", puff.Keyword(m.Role))}
		if m.Text != "" || m.Role == "user" {
			es = append(es, puff.Key("text", puff.String(m.Text)))
		}
		if len(m.Calls) > 0 {
			cs := make([]puff.Value, len(m.Calls))
			for j, c := range m.Calls {
				cs[j], _ = puff.NewMap(puff.Key("id", puff.String(c.ID)), puff.Key("fn", puff.String(c.Fn)), puff.Key("args", puff.String(c.Args)))
			}
			es = append(es, puff.Key("calls", puff.NewVector(cs...)))
		}
		if m.Role == "tool" {
			es = append(es, puff.Key("id", puff.String(m.ID)), puff.Key("result", puff.String(m.Result)))
			if m.IsError {
				es = append(es, puff.Key("error", puff.Bool(true)))
			}
		}
		vs[i], _ = puff.NewMap(es...)
	}
	return puff.NewVector(vs...)
}

// FromPuff is the transcript v, as ToPuff gives it, or why it isn't one.
func FromPuff(v puff.Value) ([]Message, error) {
	vec, ok := v.(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want a transcript, a vector of messages, got %s", puff.Print(v))
	}
	str := func(m puff.Value, k string) string {
		s, _ := puff.Field(m, k).(puff.String)
		return string(s)
	}
	ms := make([]Message, 0, vec.Len())
	for i := range vec.Len() {
		e := vec.At(i)
		role, _ := puff.Field(e, "role").(puff.Keyword)
		m := Message{Role: string(role), Text: str(e, "text"), ID: str(e, "id"), Result: str(e, "result")}
		switch m.Role {
		case "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("message %d: want :role :user, :assistant or :tool, got %s", i, puff.Print(e))
		}
		if b, _ := puff.Field(e, "error").(puff.Bool); bool(b) {
			m.IsError = true
		}
		if cs, ok := puff.Field(e, "calls").(puff.Vector); ok {
			for j := range cs.Len() {
				c := cs.At(j)
				m.Calls = append(m.Calls, Call{ID: str(c, "id"), Fn: str(c, "fn"), Args: str(c, "args")})
			}
		}
		if m.Role == "tool" && m.ID == "" {
			return nil, fmt.Errorf("message %d: a tool's answer names the call it answers, :id", i)
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// UsagePuff is u as puff data, {:input :output :cost}.
func UsagePuff(u Usage) puff.Map {
	m, _ := puff.NewMap(puff.Key("input", puff.Int(u.Input)), puff.Key("output", puff.Int(u.Output)), puff.Key("cost", puff.Float(u.Cost)))
	return m
}
