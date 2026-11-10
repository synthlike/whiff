package huff

import (
	"context"
	"fmt"
	"strings"
)

// Compaction: a transcript that grows past what a turn may send is cut
// in two, before a model call. The older part goes to the model as text,
// to be summarized; the newer part, about a quarter of the limit, stays
// as it was. The summary takes the older part's place, as the first
// message:
//
//	[{:role :user :text "Summary of the conversation so far, ..."}
//	 {:role :assistant :text "Noted; going on from there."}
//	 ...the newer part, as it was]
//
// The cut falls where a user's or assistant's message starts, never
// between a call and its answer.

// SummaryPrefix starts the message a summary takes the place of the
// older part in.
const SummaryPrefix = "Summary of the conversation so far, from earlier messages that were compacted:\n\n"

// summaryAck follows a summary when the newer part starts with a user's
// message, so the two don't follow each other.
const summaryAck = "Noted; going on from there."

const summarySystem = `You compact a conversation between a user and an AI assistant so that it can go on with less context. Write notes the assistant will read in place of the messages below: what the user wants and told it (facts, dates, names, numbers, decisions, preferences), what it did and what its tools answered that still matters, what the user asked most recently, and what is still open or in progress. Keep every detail the user may come back to; leave out pleasantries and anything settled that won't matter again. Write in the conversation's language, as plain notes, without a preamble.`

// Estimate is about how many tokens system and ms take, at four
// characters a token: a guess, not the provider's count.
func Estimate(system string, ms []Message) int {
	n := len(system)
	for _, m := range ms {
		n += len(m.Text) + len(m.Result) + 16
		for _, c := range m.Calls {
			n += len(c.Fn) + len(c.Args) + 16
		}
	}
	return n / 4
}

// cut is where to cut ms so that what follows is about keep tokens, at
// least the last whole exchange: the index of a user's or assistant's
// message, or 0 if there is nothing before it to compact.
func cut(ms []Message, keep int) int {
	at, size := 0, 0
	for i := len(ms) - 1; i > 0; i-- {
		size += Estimate("", ms[i:i+1])
		if ms[i].Role == "tool" {
			continue
		}
		if at != 0 && size > keep {
			break
		}
		at = i
	}
	return at
}

// Compact is ms with what comes before the last keep tokens or so
// summarized by the model, and what the summary took; ms itself, and
// false, if there is nothing to compact. system is the conversation's
// own system prompt, which the summarizer hears.
func Compact(ctx context.Context, p Provider, model, system string, ms []Message, keep, maxTokens int) ([]Message, Usage, bool, error) {
	at := cut(ms, keep)
	if at == 0 {
		return ms, Usage{}, false, nil
	}
	var b strings.Builder
	if system != "" {
		fmt.Fprintf(&b, "The assistant's instructions were:\n%s\n\nThe conversation:\n\n", system)
	}
	for _, m := range ms[:at] {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "User: %s\n\n", m.Text)
		case "assistant":
			if m.Text != "" {
				fmt.Fprintf(&b, "Assistant: %s\n\n", m.Text)
			}
			for _, c := range m.Calls {
				fmt.Fprintf(&b, "Assistant called %s(%s)\n\n", c.Fn, c.Args)
			}
		case "tool":
			result := m.Result
			if len(result) > 4000 {
				result = result[:4000] + "... (cut)"
			}
			what := "Tool answered"
			if m.IsError {
				what = "Tool failed"
			}
			fmt.Fprintf(&b, "%s: %s\n\n", what, result)
		}
	}
	reply, err := p.Complete(ctx, Request{Model: model, System: summarySystem, MaxTokens: maxTokens,
		Messages: []Message{{Role: "user", Text: b.String()}}})
	if err != nil {
		return ms, reply.Usage, false, fmt.Errorf("compacting the transcript: %w", err)
	}
	if strings.TrimSpace(reply.Message.Text) == "" {
		return ms, reply.Usage, false, fmt.Errorf("compacting the transcript: the model wrote no summary (it stopped: %s)", reply.Stop)
	}
	out := []Message{{Role: "user", Text: SummaryPrefix + strings.TrimSpace(reply.Message.Text)}}
	if ms[at].Role == "user" {
		out = append(out, Message{Role: "assistant", Text: summaryAck})
	}
	return append(out, ms[at:]...), reply.Usage, true, nil
}
