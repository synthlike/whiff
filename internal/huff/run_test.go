package huff

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

// script is a provider that answers each call with the next of its
// replies, keeping what it was asked.
type script struct {
	replies []Reply
	asked   []Request
}

func (s *script) Complete(_ context.Context, r Request) (Reply, error) {
	s.asked = append(s.asked, r)
	if len(s.asked) > len(s.replies) {
		return Reply{}, errors.New("no more replies")
	}
	return s.replies[len(s.asked)-1], nil
}

// box is a toolbox of todo_list, which answers, and todo_add, which is
// held for review.
type box struct{ called []string }

func (b *box) Tools() []Tool {
	return []Tool{{Name: "todo_list", Description: "Lists."}, {Name: "todo_add", Description: "Adds."}}
}

func (b *box) Call(_ context.Context, c Call) (string, bool) {
	b.called = append(b.called, c.Fn+" "+c.Args)
	switch c.Fn {
	case "todo_list":
		return `["eggs"]`, false
	case "todo_add":
		return "pending 7: waiting for your review", false
	}
	return "no tool " + c.Fn, true
}

func calls(cs ...Call) Reply {
	return Reply{Message: Message{Role: "assistant", Calls: cs}, Stop: "tools", Usage: Usage{Input: 10, Output: 2, Cost: 0.01}}
}

func says(text string) Reply {
	return Reply{Message: Message{Role: "assistant", Text: text}, Stop: "end", Usage: Usage{Input: 20, Output: 5, Cost: 0.02}}
}

// A run calls the tools the model asks for, gives it their answers, and
// ends when it answers without calling any; the transcript has it all.
func TestRun(t *testing.T) {
	p := &script{replies: []Reply{
		calls(Call{ID: "c1", Fn: "todo_list", Args: "{}"}, Call{ID: "c2", Fn: "todo_add", Args: `{"text":"\"milk\""}`}),
		calls(Call{ID: "c3", Fn: "nope", Args: "{}"}),
		says("You have eggs; milk waits for your review."),
	}}
	b := &box{}
	var steps []string
	r, err := Run(context.Background(), Turn{Provider: p, Model: "m", System: "sys", Input: "add milk", Tools: b,
		History: []Message{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "hello"}},
		Step:    func(m Message, u Usage) { steps = append(steps, m.Role) }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "You have eggs; milk waits for your review." || r.Stop != "end" || r.Calls != 3 {
		t.Errorf("result = %+v", r)
	}
	if r.Usage != (Usage{Input: 40, Output: 9, Cost: 0.04}) {
		t.Errorf("usage = %+v", r.Usage)
	}
	if got := strings.Join(b.called, "; "); got != `todo_list {}; todo_add {"text":"\"milk\""}; nope {}` {
		t.Errorf("called %s", got)
	}
	if got := strings.Join(steps, " "); got != "assistant tool tool assistant tool assistant" {
		t.Errorf("steps %s", got)
	}
	want := `[{:role :user :text "hi"} {:role :assistant :text "hello"} {:role :user :text "add milk"} ` +
		`{:calls [{:args "{}" :fn "todo_list" :id "c1"} {:args "{\"text\":\"\\\"milk\\\"\"}" :fn "todo_add" :id "c2"}] :role :assistant} ` +
		`{:id "c1" :result "[\"eggs\"]" :role :tool} {:id "c2" :result "pending 7: waiting for your review" :role :tool} ` +
		`{:calls [{:args "{}" :fn "nope" :id "c3"}] :role :assistant} {:error #t :id "c3" :result "no tool nope" :role :tool} ` +
		`{:role :assistant :text "You have eggs; milk waits for your review."}]`
	if got := puff.Print(ToPuff(r.History)); got != want {
		t.Errorf("history:\n%s\nwant\n%s", got, want)
	}
	// Each call had the system prompt, the tools and the transcript so far.
	if len(p.asked) != 3 || p.asked[2].System != "sys" || len(p.asked[2].Tools) != 2 || len(p.asked[2].Messages) != 8 {
		t.Errorf("last request = %+v", p.asked[len(p.asked)-1])
	}
}

// A run stops at its limits, saying which, with what it has.
func TestRunLimits(t *testing.T) {
	loop := func() *script {
		var rs []Reply
		for range 10 {
			rs = append(rs, calls(Call{ID: "c", Fn: "todo_list", Args: "{}"}))
		}
		return &script{replies: rs}
	}
	for _, tt := range []struct {
		lim   Limits
		stop  string
		calls int
	}{
		{Limits{Calls: 3}, "calls", 3},
		{Limits{USD: 0.025}, "usd", 3},
		{Limits{Tokens: 24}, "tokens", 2},
	} {
		r, err := Run(context.Background(), Turn{Provider: loop(), Input: "go", Tools: &box{}, Limits: tt.lim})
		if err != nil || r.Stop != tt.stop || r.Calls != tt.calls {
			t.Errorf("%+v: stop %q after %d calls, %v", tt.lim, r.Stop, r.Calls, err)
		}
	}
	// 20 model calls unless it says otherwise.
	var rs []Reply
	for range 25 {
		rs = append(rs, calls(Call{ID: "c", Fn: "todo_list", Args: "{}"}))
	}
	if r, _ := Run(context.Background(), Turn{Provider: &script{replies: rs}, Input: "go", Tools: &box{}}); r.Calls != 20 || r.Stop != "calls" {
		t.Errorf("default limit: %d calls, %q", r.Calls, r.Stop)
	}
}

// A provider's error ends the run with the transcript as far as it got;
// without tools, a call answers the model as an error.
func TestRunErrors(t *testing.T) {
	r, err := Run(context.Background(), Turn{Provider: &script{replies: []Reply{calls(Call{ID: "c", Fn: "todo_list", Args: "{}"})}}, Input: "go"})
	if err == nil || r.Calls != 1 || len(r.History) != 3 || !r.History[2].IsError || !strings.Contains(r.History[2].Result, "no tools") {
		t.Errorf("result = %+v %v", r, err)
	}
}
