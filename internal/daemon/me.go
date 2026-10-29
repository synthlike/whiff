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
			"ask": {params: []string{"question", "opts"}, optional: 1, ask: true, mark: puff.MarkRead, marked: true, check: askOpts,
				doc: `Asks you question: pending now, your answer when you reply. opts, {:choices ["yes" "no"]}, offers answers to pick.`},
			"tell": {params: []string{"text", "opts"}, optional: 1, ask: true, mark: puff.MarkRead, marked: true, check: tellOpts,
				doc: `Tells you text, a report: it waits in your inbox until you reply, which dismisses it. opts, {:actions [{:label "Show the diff" :action [:address/fn args...]}]}, offers what to do about it.`},
			"inbox": {mark: puff.MarkRead, marked: true, yours: true, native: d.meInbox,
				doc: "(ok view): what waits in your inbox, oldest first: questions to answer and reports to dismiss."},
			"answer": {params: []string{"id", "text"}, optional: 1, mark: puff.MarkWrite, marked: true, yours: true, native: d.meAnswer,
				doc: "Reply to message id in your inbox (ok text), or (ok :done) without text, which dismisses a report."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

// askOpts checks :me/ask's opts, {:choices ["yes" "no"]}, if it has any.
func askOpts(args puff.Vector) error {
	return meOpts(args, "choices", isChoices, `{:choices ["yes" "no"]}`)
}

// tellOpts checks :me/tell's opts, {:actions [...]}, if it has any.
func tellOpts(args puff.Vector) error {
	return meOpts(args, "actions", isActions, `{:actions [{:label "..." :action [:address/fn args...]}]}`)
}

// meOpts checks the opts in args, a map whose one key is key, which want
// checks; example shows one.
func meOpts(args puff.Vector, key string, want check, example string) error {
	if args.Len() < 2 {
		return nil
	}
	opts, ok := args.At(1).(puff.Map)
	if !ok {
		return fmt.Errorf("want opts as a map, %s, got %s", example, puff.Print(args.At(1)))
	}
	for i := range opts.Len() {
		e := opts.Entry(i)
		if !puff.Equal(e.Key, puff.Keyword(key)) {
			return fmt.Errorf("opts have no %s: want %s", puff.Print(e.Key), example)
		}
		if w := want(e.Val); w != "" {
			return fmt.Errorf("want :%s as %s, got %s", key, w, puff.Print(e.Val))
		}
	}
	return nil
}

func isChoices(v puff.Value) string {
	vec, ok := v.(puff.Vector)
	for i := range vec.Len() {
		_, isStr := vec.At(i).(puff.String)
		ok = ok && isStr
	}
	if !ok || vec.Len() == 0 {
		return "a vector of strings"
	}
	return ""
}

// meChanged publishes :me/changed, with how many wait in :me's inbox, each
// time it changes, for a client showing it; user is the origin of the
// chain that changed it.
func (st *state) meChanged(user bool) {
	st.publish(message{from: "me", topic: "me/changed", args: puff.Print(puff.NewVector(puff.Int(len(st.inbox("me"))))), user: user})
}

// meInbox is :me's inbox as (items [...]): a question with its choices and
// Answer, which asks for your answer, and a report with its actions and
// Dismiss.
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
// it, and how to answer it: a question's choices, each answering with
// itself, and Answer, or Other if it has choices; a report's actions, and
// Dismiss.
func (a *asked) meItem() puff.Value {
	text := puff.Print(a.args.At(0))
	if s, ok := a.args.At(0).(puff.String); ok {
		text = string(s)
	}
	// opts are its choices or actions, checked when it came.
	var opts puff.Vector
	if a.args.Len() > 1 {
		key := map[string]string{"ask": "choices", "tell": "actions"}[a.fn]
		if v, ok := a.args.At(1).(puff.Map).Get(puff.Keyword(key)); ok {
			opts = v.(puff.Vector)
		}
	}
	from := "client"
	if a.from != "" {
		from = ":" + a.from
	}
	answer := func(label string, args ...puff.Value) []puff.Entry {
		return []puff.Entry{
			{Key: puff.Keyword("label"), Val: puff.String(label)},
			{Key: puff.Keyword("action"), Val: puff.NewVector(append([]puff.Value{puff.Keyword("me/answer"), puff.Int(a.id)}, args...)...)},
		}
	}
	var actions []puff.Value
	verb := "tells you"
	if a.fn == "ask" {
		verb = "asks"
		for i := range opts.Len() {
			c := opts.At(i).(puff.String)
			m, _ := puff.NewMap(answer(string(c), c)...)
			actions = append(actions, m)
		}
		other := "Answer"
		if opts.Len() > 0 {
			other = "Other"
		}
		m, _ := puff.NewMap(append(answer(other), puff.Entry{Key: puff.Keyword("ask"), Val: puff.String("Your answer")})...)
		actions = append(actions, m)
	} else {
		// Clicking an action sends it as you, so one an agent's chain
		// offers is labelled with its whole message: its label could say
		// anything.
		for i := range opts.Len() {
			act := opts.At(i).(puff.Map)
			if !a.user {
				msg, _ := act.Get(puff.Keyword("action"))
				act, _ = act.With(puff.Keyword("label"), puff.String(puff.Print(msg)))
			}
			actions = append(actions, act)
		}
		m, _ := puff.NewMap(answer("Dismiss")...)
		actions = append(actions, m)
	}
	m, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("label"), Val: puff.String(text)},
		puff.Entry{Key: puff.Keyword("detail"), Val: puff.String(fmt.Sprintf("%d: %s %s", a.id, from, verb))},
		puff.Entry{Key: puff.Keyword("actions"), Val: puff.NewVector(actions...)},
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
