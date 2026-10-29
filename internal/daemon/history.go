package daemon

import (
	"fmt"
	"slices"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// historySize bounds history, which is in memory until it is stored.
const historySize = 1000

// entry is one line of history: a message and its Result, or a note about
// whiff itself, such as a service that failed to load.
type entry struct {
	at    time.Time
	kind  string // "call", "send", "pub", "done" or "eval", an agent's that writes
	id    int64  // the message's, or the eval's
	cause int64  // what led to it: see rpc.Entry
	user  bool   // the origin: whether the chain began with the user
	from  string // the sender's address; "" for a client
	to    string // address/fn, or address on :topic; "" for a client
	args  string // a printed vector
	reply string // the printed Result; "" for a done no service handled
	took  time.Duration
	gate  string // how the review gate treated an agent's write: see rpc.Entry
	note  string
}

// rpc is e as clients get it.
func (e entry) rpc() rpc.Entry {
	if e.note != "" {
		return rpc.Entry{At: e.at, Kind: "note", Note: e.note}
	}
	return rpc.Entry{At: e.at, Kind: e.kind, ID: e.id, Cause: e.cause, Origin: string(origin(e.user)), From: e.from, To: e.to,
		Args: e.args, Reply: e.reply, Took: e.took, Gate: e.gate}
}

// tracer is a client following history: whiff history -f, sniff. Its
// entries wait in ch for the client's connection; when ch is full they are
// dropped, so a slow client never holds up a message.
type tracer struct {
	ch chan rpc.Entry
}

func (st *state) record(e entry) {
	st.history = append(st.history, e)
	if n := len(st.history) - historySize; n > 0 {
		st.history = slices.Delete(st.history, 0, n)
	}
	if len(st.tracers) == 0 {
		return
	}
	re := e.rpc()
	for t := range st.tracers {
		select {
		case t.ch <- re:
		default:
		}
	}
}

// note records something about whiff itself in history, and in the log.
func (d *daemon) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(d.log, "whiff: %s\n", msg)
	d.do(func(st *state) { st.record(entry{at: time.Now(), note: msg}) })
}

// entries is history so far, oldest first.
func (st *state) entries() []rpc.Entry {
	es := make([]rpc.Entry, len(st.history))
	for i, e := range st.history {
		es[i] = e.rpc()
	}
	return es
}

// trace sends l's client each new entry of history until the connection
// ends. The Response holds history so far: taken with the subscription in
// one step, so no entry is missed or repeated between them.
func (d *daemon) trace(l *link) (*tracer, rpc.Response) {
	t := &tracer{ch: make(chan rpc.Entry, mailboxSize)}
	var resp rpc.Response
	d.do(func(st *state) {
		resp.History = st.entries()
		st.tracers[t] = true
	})
	go func() {
		for {
			select {
			case e := <-t.ch:
				if l.conn.Send(rpc.Response{Entry: &e}) != nil {
					return
				}
			case <-l.gone:
				return
			}
		}
	}()
	return t, resp
}

// untrace ends t.
func (d *daemon) untrace(t *tracer) {
	d.do(func(st *state) { delete(st.tracers, t) })
}
