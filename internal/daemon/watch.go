package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// stepsShown is how many of a running job's last lines tasks/watch shows.
const stepsShown = 200

// watch is task id as it runs, for tasks/watch: (ok view), a running
// job's lines as items, its last stepsShown, oldest first, claude -p's
// stream-json read as what the turn says and calls and what answers it.
// One that isn't running is its item in :tasks/view, after the step its
// claude result says, if it has one. It follows :jobs/<id>, which a
// :stream job publishes each line on, and :tasks/changed, for its end.
func (d *daemon) watch(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a task id, got %s", puff.Print(args[0]))
	}
	var p *proc
	d.do(func(st *state) {
		if j := st.jobs[int64(id)]; j != nil {
			p = j.p
		}
	})
	var items []puff.Value
	if p != nil {
		out := p.stdout.String()
		ls := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(out) >= tailSize-4 {
			ls = ls[1:] // cut: its first line is only the end of one
		}
		for _, l := range ls {
			items = append(items, step(l)...)
		}
		if len(items) > stepsShown {
			items = items[len(items)-stepsShown:]
		}
		if len(items) == 0 {
			items = append(items, stepItem("running", "nothing printed yet", ""))
		}
	} else {
		now := time.Now()
		ts := d.tasks(func(t task) bool { return t.id == int64(id) }, 1, now)
		if len(ts) == 0 {
			return puff.Errorf("no-id", "no task %d is open, or ended in the last day", id), nil
		}
		var l streamLine
		if out, ok := resultOut(ts[0].result); ok && json.Unmarshal([]byte(out), &l) == nil && l.Type == "result" {
			items = append(items, step(out)...)
		}
		items = append(items, ts[0].item(now))
	}
	opts, _ := puff.NewMap(puff.Key("changed", puff.NewVector(puff.Keyword(fmt.Sprintf("jobs/%d", id)), puff.Keyword("tasks/changed"))))
	return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
}

// resultOut is the stdout of r, a job's result: (ok text), as :as :text
// gives, or (ok {:stdout text}).
func resultOut(r puff.Value) (string, bool) {
	tag, ok := r.(puff.Tag)
	if !ok || tag.Name() != "ok" || tag.Len() != 1 {
		return "", false
	}
	out, ok := tag.At(0).(puff.String)
	if !ok {
		out, ok = puff.Field(tag.At(0), "stdout").(puff.String)
	}
	return string(out), ok
}

// streamLine is what step reads of a line of claude -p's stream-json.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
			IsError  bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
	Result   string   `json:"result"`
	Cost     *float64 `json:"total_cost_usd"`
	Turns    int      `json:"num_turns"`
	IsError  bool     `json:"is_error"`
	Errors   []string `json:"errors"`
	Duration int      `json:"duration_ms"`
}

// step is line, one a job printed, as items: claude's stream-json as
// what the turn said, called and was answered, a message's blocks each
// one, and how it ended; any other line as it is. None for one with
// nothing to show, as a rate limit's.
func step(line string) []puff.Value {
	var l streamLine
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &l) != nil || l.Type == "" {
		if strings.TrimSpace(line) == "" {
			return nil
		}
		return []puff.Value{stepItem("", line, line)}
	}
	switch l.Type {
	case "system":
		if l.Subtype != "init" {
			return nil
		}
		return []puff.Value{stepItem("began", l.Model, "")}
	case "assistant", "user":
		var vs []puff.Value
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				vs = append(vs, stepItem("says", c.Text, c.Text))
			case "thinking":
				if c.Thinking != "" {
					vs = append(vs, stepItem("thinks", c.Thinking, c.Thinking))
				}
			case "tool_use":
				in := string(c.Input)
				vs = append(vs, stepItem("calls", toolName(c.Name)+" "+in, in))
			case "tool_result":
				text := toolText(c.Content)
				word := "answer"
				if c.IsError {
					word = "error"
				}
				vs = append(vs, stepItem(word, text, text))
			}
		}
		return vs
	case "result":
		word := "ended"
		text := l.Result
		if l.IsError {
			word = "failed"
			if text == "" {
				text = strings.Join(l.Errors, "; ")
			}
		}
		var how []string
		if l.Cost != nil {
			how = append(how, fmt.Sprintf("$%.2f", *l.Cost))
		}
		switch {
		case l.Turns == 1:
			how = append(how, "1 turn")
		case l.Turns > 1:
			how = append(how, fmt.Sprintf("%d turns", l.Turns))
		}
		if l.Duration > 0 {
			how = append(how, (time.Duration(l.Duration) * time.Millisecond).Round(time.Second).String())
		}
		if len(how) > 0 {
			word += " (" + strings.Join(how, ", ") + ")"
		}
		return []puff.Value{stepItem(word, text, text)}
	}
	return nil
}

// stepItem is an item: word and text's first line, cut, as its label,
// and detail, cut to 2000 characters, if it says more.
func stepItem(word, text, detail string) puff.Value {
	label := short(text)
	if word != "" {
		label = word + "  " + label
	}
	es := []puff.Entry{puff.Key("label", puff.String(strings.TrimSpace(label)))}
	if r := []rune(detail); len(r) > 2000 {
		detail = string(r[:1999]) + "…"
	}
	if detail != "" && detail != firstLine(detail) || len([]rune(firstLine(detail))) > 120 {
		es = append(es, puff.Key("detail", puff.String(detail)))
	}
	m, _ := puff.NewMap(es...)
	return m
}

// toolName is a tool claude called, without whiff mcp's prefix: whiff's
// own are addr_fn, as gh_prs.
func toolName(name string) string {
	return strings.TrimPrefix(name, "mcp__whiff__")
}

// toolText is a tool result's content, a string or [{type text}], as
// text.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		} else {
			b.WriteString("[" + p.Type + "]")
		}
	}
	return b.String()
}
