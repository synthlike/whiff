package daemon

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// Timers, every send-after waiting for its time, are whiff's own address
// :timers: as data, as a view, and cancel, which is yours. A timer is its
// stored record, so cancelling one deletes it, and arm sends only one still
// stored. An agent's :every and its later notes are timers too.

// timerID is the number in t's key, after its due time: its id in :timers.
func timerID(key []byte) int64 {
	if len(key) < 16 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(key[8:16]))
}

// storedTimers are the timers stored, soonest first.
func (d *daemon) storedTimers() []*timer {
	var ts []*timer
	d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(timersBucket).ForEach(func(k, v []byte) error {
			if t, err := storedTimer(slices.Clone(k), string(v)); err == nil {
				ts = append(ts, t)
			}
			return nil
		})
	})
	return ts
}

// fnName is what t sends: its function, or :topic for a publish.
func (t *timer) fnName() string {
	if t.m.topic != "" {
		return ":" + t.m.topic
	}
	return t.m.fn
}

// value is t as :timers/list gives it.
func (t *timer) value() puff.Value {
	args, _ := puff.ReadValue(t.m.args)
	es := []puff.Entry{
		puff.Key("id", puff.Int(timerID(t.key))),
		puff.Key("to", puff.Keyword(t.addr)),
		puff.Key("fn", puff.Keyword(t.fnName())),
		puff.Key("args", args),
		puff.Key("due", puff.NewTime(t.due)),
	}
	if t.m.from != "" {
		es = append(es, puff.Key("from", puff.Keyword(t.m.from)))
	}
	m, _ := puff.NewMap(es...)
	return m
}

// item is t as an item of :timers/view: when, and what it sends to whom,
// then who set it and its arguments; Cancel.
func (t *timer) item(now time.Time) puff.Value {
	when := "due now"
	if left := t.due.Sub(now); left > 0 {
		when = "in " + roughly(left)
	}
	label := when + "  :" + t.addr + "/" + strings.TrimPrefix(t.fnName(), ":")
	var parts []string
	switch {
	case t.m.from == "":
		parts = append(parts, "set by you")
	case t.own():
		parts = append(parts, "set by itself")
	default:
		parts = append(parts, "set by :"+t.m.from)
	}
	parts = append(parts, t.due.Local().Format("Mon 2 Jan 15:04"))
	if args, _ := puff.ReadValue(t.m.args); args != nil {
		if v, ok := args.(puff.Vector); ok && v.Len() > 0 {
			parts = append(parts, short(puff.Print(args)))
		}
	}
	cancel, _ := puff.NewMap(puff.Key("label", puff.String("Cancel")), puff.Key("action", puff.NewVector(puff.Keyword("timers/cancel"), puff.Int(timerID(t.key)))))
	m, _ := puff.NewMap(
		puff.Key("label", puff.String(label)),
		puff.Key("detail", puff.String(strings.Join(parts, " · "))),
		puff.Key("actions", puff.NewVector(cancel)),
	)
	return m
}

// roughly is d for people, to the minute, or seconds under one: 40s,
// 5m, 2h10m, 3d4h.
func roughly(d time.Duration) string {
	if d < time.Minute-time.Second/2 {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int((d - time.Duration(h)*time.Hour).Minutes()); m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	days := int(d.Hours()) / 24
	if h := int(d.Hours()) % 24; h > 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	return fmt.Sprintf("%dd", days)
}

// timersChanged publishes :timers/changed, as a timer is set, fires or is
// cancelled: on a goroutine of its own, as it is called on the route
// goroutine too, as resume forgets a timer. It only says to look again,
// so its order doesn't matter.
func (d *daemon) timersChanged() {
	go d.publish(message{from: "timers", topic: "timers/changed", args: "[:changed]", lineage: lineage{user: true}})
}

// stillSet says whether t is stored still: not cancelled. One that isn't
// stored, as whiff try's, can't be.
func (d *daemon) stillSet(t *timer) bool {
	if t.key == nil {
		return true
	}
	set := false
	d.db.View(func(tx *bbolt.Tx) error {
		set = tx.Bucket(timersBucket).Get(t.key) != nil
		return nil
	})
	return set
}

// cancelTimer deletes timer id: (ok :done), or (err {:kind :no-id}). One
// that is firing as you cancel it may still be delivered.
func (d *daemon) cancelTimer(id int64) puff.Value {
	found := false
	err := d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(timersBucket)
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if timerID(k) == id {
				found = true
				return b.Delete(k)
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return puff.Errorf("store", "%v", err)
	case !found:
		return puff.Errorf("no-id", "no timer %d waits", id)
	}
	d.timersChanged()
	return puff.Done
}

// timersService is :timers.
func (d *daemon) timersService() *service {
	return &service{name: "timers", d: d, cur: caller{addr: "timers"},
		fns: map[string]handler{
			"list": {mark: puff.MarkRead, marked: true,
				doc: "(ok timers): every send-after waiting for its time, soonest first, each {:id :to :fn :args :due :from}: what it sends to whom, when, and who set it (none for you). An agent's :every and its later notes are timers too.",
				native: func(message, []puff.Value) (puff.Value, error) {
					var vs []puff.Value
					for _, t := range d.storedTimers() {
						vs = append(vs, t.value())
					}
					return puff.Ok(puff.NewVector(vs...)), nil
				}},
			"view": {mark: puff.MarkRead, marked: true,
				doc: "(ok view): the timers as list gives them, as items, soonest first, each with Cancel; following :timers/changed, which is published as one is set, fires or is cancelled.",
				native: func(message, []puff.Value) (puff.Value, error) {
					now := time.Now()
					var items []puff.Value
					for _, t := range d.storedTimers() {
						items = append(items, t.item(now))
					}
					opts, _ := puff.NewMap(puff.Key("changed", puff.Keyword("timers/changed")))
					return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
				}},
			"cancel": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Cancel timer id, so it never sends its message: :done, or (err {:kind :no-id}) if none waits. One firing as you cancel it may still be delivered. A service's timers to itself that its :start sets, as an agent's :every and later notes, come back when it loads again.",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					id, ok := args[0].(puff.Int)
					if !ok {
						return nil, fmt.Errorf("want a timer id, got %s", puff.Print(args[0]))
					}
					return d.cancelTimer(int64(id)), nil
				}},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}
