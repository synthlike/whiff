package huff

import (
	"context"
	"strings"
	"testing"
)

// big is text of about n tokens, as Estimate counts them.
func big(word string, n int) string { return strings.Repeat(word+" ", n*4/(len(word)+1)) }

// A turn whose transcript is past Compact summarizes the older part with
// the model, keeps the newer part as it was, and carries on with the
// summary in the older part's place; the summary's cost is the turn's,
// and Full keeps what was summarized. The cut never parts a call from its
// answer.
func TestCompact(t *testing.T) {
	history := []Message{
		{Role: "user", Text: "The release is on Friday. " + big("old", 300)},
		{Role: "assistant", Text: "Noted. " + big("old", 300)},
		{Role: "user", Text: "List my todos."},
		{Role: "assistant", Calls: []Call{{ID: "c1", Fn: "todo_list", Args: "{}"}}},
		{Role: "tool", ID: "c1", Result: big("newer", 100)},
		{Role: "assistant", Text: "Eggs."},
	}
	p := &script{replies: []Reply{says("Release Friday; the user keeps todos."), says("Friday.")}}
	var heard []Usage
	r, err := Run(context.Background(), Turn{Provider: p, Model: "m", System: "Help.", History: history,
		Input: "When is the release?", Compact: 600, Compacted: func(u Usage) { heard = append(heard, u) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 2 {
		t.Fatalf("%d model calls, want the summary's and the turn's", len(p.asked))
	}
	sum := p.asked[0]
	if sum.Tools != nil || !strings.Contains(sum.System, "compact") || len(sum.Messages) != 1 {
		t.Errorf("the summary's request = %+v", sum)
	}
	asked := sum.Messages[0].Text
	for _, want := range []string{"instructions were:\nHelp.", "User: The release is on Friday.", "Assistant: Noted."} {
		if !strings.Contains(asked, want) {
			t.Errorf("the summarizer wasn't told %q", want)
		}
	}
	if strings.Contains(asked, "List my todos") || strings.Contains(asked, "newer") {
		t.Errorf("the summarizer was told the newer part:\n%s", asked)
	}
	want := []string{
		"user " + SummaryPrefix + "Release Friday; the user keeps todos.",
		"assistant " + summaryAck,
		"user List my todos.", "assistant ", "tool ", "assistant Eggs.",
		"user When is the release?", "assistant Friday.",
	}
	got := func(ms []Message) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Role+" "+m.Text)
		}
		return out
	}
	if g := got(r.History); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("history =\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
	if g := got(p.asked[1].Messages); strings.Join(g, "\n") != strings.Join(want[:7], "\n") {
		t.Errorf("the turn's model call was sent\n%s", strings.Join(g, "\n"))
	}
	if r.Compacted != 1 || len(r.Full) != len(history)+2 || r.Full[0].Text != history[0].Text {
		t.Errorf("compacted %d, full %d messages", r.Compacted, len(r.Full))
	}
	if r.Usage.Cost != 0.04 || len(heard) != 1 || heard[0].Cost != 0.02 {
		t.Errorf("usage %+v, heard %+v", r.Usage, heard)
	}
}

// Below Compact, or with it 0, nothing is summarized, and Full is nil.
func TestNoCompact(t *testing.T) {
	history := []Message{{Role: "user", Text: big("old", 600)}, {Role: "assistant", Text: "ok"}}
	for _, limit := range []int{0, 10000} {
		p := &script{replies: []Reply{says("hi")}}
		r, err := Run(context.Background(), Turn{Provider: p, History: history, Input: "hello", Compact: limit})
		if err != nil || len(p.asked) != 1 || r.Compacted != 0 || r.Full != nil || len(r.History) != 4 {
			t.Errorf("Compact %d: %d calls, compacted %d, %v", limit, len(p.asked), r.Compacted, err)
		}
	}
}

// The cut falls where a user's or assistant's message starts, keeping at
// least the last one whole, however big; with only one, there is nothing
// to compact. A failed summary ends the turn, its transcript as it was.
func TestCut(t *testing.T) {
	ms := []Message{
		{Role: "user", Text: "a"},
		{Role: "assistant", Calls: []Call{{ID: "1"}}},
		{Role: "tool", ID: "1", Result: big("x", 5000)},
		{Role: "tool", ID: "1", Result: "y"},
	}
	if at := cut(ms, 10); at != 1 {
		t.Errorf("cut = %d, want 1, the assistant's call with its answers", at)
	}
	if at := cut(ms[:1], 10); at != 0 {
		t.Errorf("cut of one message = %d, want 0", at)
	}
	p := &script{}
	r, err := Run(context.Background(), Turn{Provider: p, History: ms, Input: "go on", Compact: 100})
	if err == nil || !strings.Contains(err.Error(), "compacting") || len(r.History) != 5 {
		t.Errorf("a failed summary: %v, %d messages", err, len(r.History))
	}
}
