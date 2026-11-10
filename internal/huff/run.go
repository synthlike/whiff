package huff

import (
	"context"
	"fmt"
)

// A Toolbox is what a run's model may call: whiff functions, through a
// session, or a test's.
type Toolbox interface {
	Tools() []Tool
	// Call runs c, answering its result as text for the model, and
	// whether the call failed: a failed call answers the model, it doesn't
	// end the run.
	Call(ctx context.Context, c Call) (text string, isError bool)
}

// Limits bound a run: how many model calls it makes (20 if 0), and, if
// not 0, the dollars it may spend, as the provider reports cost, and the
// tokens, in and out.
type Limits struct {
	Calls  int
	USD    float64
	Tokens int
}

// A Turn is one run: a model on a provider, a system prompt, the
// transcript so far, the user's new input, the tools, nil for none, and
// its limits. Step, if not nil, hears each message as the run adds it,
// with what the model call that wrote it took.
type Turn struct {
	Provider Provider
	Model    string
	System   string
	History  []Message
	Input    string
	Tools    Toolbox
	Limits   Limits
	Step     func(m Message, u Usage)
	// MaxTokens bounds each reply, 0 for the provider's default. OpenRouter
	// holds back credit for each call in flight as if its reply were that
	// long, so a model's whole limit can refuse calls the credit would pay.
	MaxTokens int
	// Compact, if not 0, is how many tokens, as Estimate guesses them, a
	// model call may send before the transcript is compacted (compact.go),
	// keeping about a quarter of it as it was. Compacted, if not nil,
	// hears that it was, with what the summary took.
	Compact   int
	Compacted func(u Usage)
}

// A Result is how a run ended: the model's last text, the transcript with
// the run's messages, what it took, summaries included, how many model
// calls it made, and why it stopped: "end", "length" or "filter" as the
// model's last reply said, or "calls", "usd" or "tokens" for the limit it
// reached. If the run compacted the transcript, Compacted says how many
// times, and Full is the transcript as it would be without, to keep.
type Result struct {
	Text      string
	History   []Message
	Usage     Usage
	Calls     int
	Stop      string
	Compacted int
	Full      []Message
}

// Run runs t: it calls the model, runs the tools it asks for, gives it
// their answers, and goes on until it answers without calling any, or
// reaches a limit. An error from the provider ends it, with the result so
// far, the transcript as far as it got.
func Run(ctx context.Context, t Turn) (r Result, err error) {
	maxCalls := t.Limits.Calls
	if maxCalls <= 0 {
		maxCalls = 20
	}
	step := t.Step
	if step == nil {
		step = func(Message, Usage) {}
	}
	r = Result{History: append(append([]Message(nil), t.History...), Message{Role: "user", Text: t.Input})}
	full := append([]Message(nil), r.History...)
	add := func(m Message) {
		r.History = append(r.History, m)
		full = append(full, m)
	}
	defer func() {
		if r.Compacted > 0 {
			r.Full = full
		}
	}()
	var tools []Tool
	if t.Tools != nil {
		tools = t.Tools.Tools()
	}
	for {
		if r.Calls >= maxCalls {
			r.Stop = "calls"
			return r, nil
		}
		if t.Compact > 0 && Estimate(t.System, r.History) > t.Compact {
			ms, u, ok, err := Compact(ctx, t.Provider, t.Model, t.System, r.History, t.Compact/4, t.MaxTokens)
			r.Usage = r.Usage.Add(u)
			if err != nil {
				return r, err
			}
			if ok {
				r.History = append([]Message(nil), ms...)
				r.Compacted++
				if t.Compacted != nil {
					t.Compacted(u)
				}
			}
		}
		reply, err := t.Provider.Complete(ctx, Request{Model: t.Model, System: t.System, Messages: r.History, Tools: tools, MaxTokens: t.MaxTokens})
		if err != nil {
			return r, err
		}
		r.Calls++
		r.Usage = r.Usage.Add(reply.Usage)
		add(reply.Message)
		if reply.Message.Text != "" {
			r.Text = reply.Message.Text
		}
		step(reply.Message, reply.Usage)
		if reply.Stop != "tools" || len(reply.Message.Calls) == 0 {
			r.Stop = reply.Stop
			return r, nil
		}
		for _, c := range reply.Message.Calls {
			m := Message{Role: "tool", ID: c.ID}
			if t.Tools == nil {
				m.Result, m.IsError = fmt.Sprintf("there are no tools: %s isn't one", c.Fn), true
			} else {
				m.Result, m.IsError = t.Tools.Call(ctx, c)
			}
			add(m)
			step(m, Usage{})
		}
		switch {
		case t.Limits.USD > 0 && r.Usage.Cost >= t.Limits.USD:
			r.Stop = "usd"
			return r, nil
		case t.Limits.Tokens > 0 && r.Usage.Input+r.Usage.Output >= t.Limits.Tokens:
			r.Stop = "tokens"
			return r, nil
		}
	}
}
