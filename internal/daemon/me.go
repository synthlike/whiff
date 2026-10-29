package daemon

import (
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
)

// meService is :me, your own address: its functions are asks, so what is
// sent to it waits in its inbox for you to answer with whiff reply. It is
// how a service or an agent asks you something, or tells you. They are
// :read: they change nothing of yours, and you see each one before it does
// anything, so the review gate has nothing to add. Its inbox is a view too,
// whose actions answer, for any client that sends messages; those are
// yours, as only you answer :me.
func (d *daemon) meService() *service {
	return &service{name: "me", d: d, cur: caller{addr: "me"},
		fns: map[string]handler{
			"ask": {params: []string{"question"}, ask: true, mark: puff.MarkRead, marked: true,
				doc: "Asks you question: pending now, your answer when you reply."},
			"tell": {params: []string{"text"}, ask: true, mark: puff.MarkRead, marked: true,
				doc: "Tells you text, a report: it waits in your inbox until you reply, which dismisses it."},
			"inbox": {mark: puff.MarkRead, marked: true, yours: true, native: d.meInbox,
				doc: "(ok view): what waits in your inbox, oldest first: questions to answer and reports to dismiss."},
			"answer": {params: []string{"id", "text"}, optional: 1, mark: puff.MarkWrite, marked: true, yours: true, native: d.meAnswer,
				doc: "Reply to message id in your inbox (ok text), or (ok :done) without text, which dismisses a report."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

// meChanged publishes :me/changed, with how many wait in :me's inbox, each
// time it changes, for a client showing it; user is the origin of the
// chain that changed it.
func (st *state) meChanged(user bool) {
	st.publish(message{from: "me", topic: "me/changed", args: puff.Print(puff.NewVector(puff.Int(len(st.inbox("me"))))), user: user})
}

// meInbox is :me's inbox as (items [...]): a question with Answer, which
// asks for your answer, and a report with Dismiss.
func (d *daemon) meInbox(message, []puff.Value) (puff.Value, error) {
	var items []puff.Value
	d.do(func(st *state) {
		for _, a := range st.inbox("me") {
			items = append(items, a.meItem())
		}
	})
	opts, _ := puff.NewMap(puff.Entry{Key: puff.Keyword("changed"), Val: puff.Keyword("me/changed")})
	return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
}

// meItem is a, in :me's inbox, as an item of :me/inbox: its text, who sent
// it, and how to answer it.
func (a *asked) meItem() puff.Value {
	label := puff.Print(a.args)
	if s, ok := a.args.At(0).(puff.String); ok && a.args.Len() == 1 {
		label = string(s)
	}
	from := "client"
	if a.from != "" {
		from = ":" + a.from
	}
	es := []puff.Entry{
		{Key: puff.Keyword("label"), Val: puff.String("Dismiss")},
		{Key: puff.Keyword("action"), Val: puff.NewVector(puff.Keyword("me/answer"), puff.Int(a.id))},
	}
	verb := "tells you"
	if a.fn == "ask" {
		verb = "asks"
		es[0].Val = puff.String("Answer")
		es = append(es, puff.Entry{Key: puff.Keyword("ask"), Val: puff.String("Your answer")})
	}
	action, _ := puff.NewMap(es...)
	m, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("label"), Val: puff.String(label)},
		puff.Entry{Key: puff.Keyword("detail"), Val: puff.String(fmt.Sprintf("%d: %s %s", a.id, from, verb))},
		puff.Entry{Key: puff.Keyword("actions"), Val: puff.NewVector(action)},
	)
	return m
}

func (d *daemon) meAnswer(_ message, args []puff.Value) (puff.Value, error) {
	result := puff.Done
	if len(args) > 1 {
		text, ok := args[1].(puff.String)
		if !ok {
			return nil, fmt.Errorf("want your answer as a string, got %s", puff.Print(args[1]))
		}
		result = puff.Ok(text)
	}
	return d.reply("me", true, args[0], result)
}
