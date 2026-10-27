package daemon

import (
	"github.com/synthlike/whiff/internal/puff"
)

// meService is :me, your own address: its functions are asks, so what is
// sent to it waits in its inbox for you to answer with whiff reply. It is
// how a service or an agent asks you something, or tells you. They are
// :read: they change nothing of yours, and you see each one before it does
// anything, so the review gate has nothing to add.
func (d *daemon) meService() *service {
	return &service{name: "me", d: d, cur: caller{addr: "me"},
		fns: map[string]handler{
			"ask": {params: []string{"question"}, ask: true, mark: puff.MarkRead, marked: true,
				doc: "Asks you question: pending now, your answer when you reply."},
			"tell": {params: []string{"text"}, ask: true, mark: puff.MarkRead, marked: true,
				doc: "Tells you text, a report: it waits in your inbox until you reply, which dismisses it."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}
