package daemon

import (
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// historySize bounds the history kept in memory, which clients get: the
// last entries of what whiff.db holds.
const historySize = 1000

// historyBucket holds every entry of history, printed, under the bucket's
// sequence, so it reads back in the order it was recorded. Entries older
// than historyKept are deleted at start.
var historyBucket = []byte("history")

// msgsKey, in the meta bucket, is the last id given to a message in
// history, so ids, and the chains they make, don't begin again at a
// restart.
var msgsKey = []byte("msgs")

// historyKept is how long history is kept, as results are.
const historyKept = resultKept

// historyDelay is how long the history writer waits, once an entry is
// recorded, for more to store with it.
const historyDelay = 100 * time.Millisecond

// unstoredSize bounds the history waiting for the writer.
const unstoredSize = 10000

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

// stored is e as whiff.db holds it: a map of the fields it has. Args and
// reply stay the text they were printed as.
func (e entry) stored() puff.Value {
	es := []puff.Entry{{Key: puff.Keyword("at"), Val: puff.NewTime(e.at)}}
	add := func(k string, v puff.Value) { es = append(es, puff.Entry{Key: puff.Keyword(k), Val: v}) }
	if e.note != "" {
		add("note", puff.String(e.note))
	} else {
		add("kind", puff.String(e.kind))
		add("id", puff.Int(e.id))
		add("origin", origin(e.user))
		add("took", puff.Duration(e.took))
	}
	for _, f := range []struct{ k, v string }{{"from", e.from}, {"to", e.to}, {"args", e.args}, {"reply", e.reply}, {"gate", e.gate}} {
		if f.v != "" {
			add(f.k, puff.String(f.v))
		}
	}
	if e.cause != 0 {
		add("cause", puff.Int(e.cause))
	}
	m, _ := puff.NewMap(es...)
	return m
}

// storedEntry is the entry p, as stored printed it.
func storedEntry(p string) (entry, error) {
	get, err := fields(p)
	if err != nil {
		return entry{}, err
	}
	at, ok := get("at").(puff.Time)
	if !ok {
		return entry{}, fmt.Errorf("no :at in %s", p)
	}
	str := func(k string) string { s, _ := get(k).(puff.String); return string(s) }
	num := func(k string) int64 { n, _ := get(k).(puff.Int); return int64(n) }
	took, _ := get("took").(puff.Duration)
	return entry{at: at.Go(), kind: str("kind"), id: num("id"), cause: num("cause"), user: get("origin") == puff.Keyword("user"),
		from: str("from"), to: str("to"), args: str("args"), reply: str("reply"), took: time.Duration(took), gate: str("gate"), note: str("note")}, nil
}

// batch is history on its way to whiff.db: entries, the last message id
// given out as they were taken, and how many were dropped before them.
type batch struct {
	es      []entry
	last    int64
	dropped int
}

// takeUnstored is the history recorded since it was last taken.
func (st *state) takeUnstored() batch {
	b := batch{st.unstored, st.lastMsg, st.dropped}
	st.unstored, st.dropped = nil, 0
	return b
}

// tracer is a client following history: whiff history -f, sniff. Its
// entries wait in ch for the client's connection; when ch is full they are
// dropped, so a slow client never holds up a message.
type tracer struct {
	ch chan rpc.Entry
}

// record adds e to history: in memory, to whiff.db, and to the tracers. It
// waits in st.unstored for the history writer, so a message never waits
// for the disk; if whiff.db falls unstoredSize behind, e isn't stored, and
// the log says so.
func (st *state) record(e entry) {
	st.history = append(st.history, e)
	if n := len(st.history) - historySize; n > 0 {
		st.history = slices.Delete(st.history, 0, n)
	}
	if len(st.unstored) >= unstoredSize {
		st.dropped++
	} else if st.unstored = append(st.unstored, e); len(st.unstored) == 1 {
		// Only the first wakes the writer, which takes the rest with it.
		select {
		case st.kick <- struct{}{}:
		default:
		}
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

// writeHistory stores history in whiff.db: woken by kick, it waits
// historyDelay for more, then takes what waits in one transaction. final
// is what was left as the route goroutine stopped, which it stores last.
// So at most historyDelay of history is lost if whiff dies.
func (d *daemon) writeHistory(kick <-chan struct{}, final <-chan batch) {
	for {
		select {
		case <-kick:
		case b := <-final:
			d.storeBatch(b)
			return
		}
		select {
		case <-time.After(historyDelay):
		case b := <-final:
			d.storeBatch(b)
			return
		}
		var b batch
		if !d.do(func(st *state) { b = st.takeUnstored() }) {
			d.storeBatch(<-final)
			return
		}
		d.storeBatch(b)
	}
}

// storeBatch stores b, saying in the log what wasn't stored.
func (d *daemon) storeBatch(b batch) {
	if b.dropped > 0 {
		fmt.Fprintf(d.log, "whiff: %d history entries weren't stored, as whiff.db fell behind\n", b.dropped)
	}
	if len(b.es) == 0 {
		return
	}
	if err := storeEntries(d.db, b.es, b.last); err != nil {
		fmt.Fprintf(d.log, "whiff: %d history entries weren't stored: %v\n", len(b.es), err)
	}
}

// storeEntries stores es in one transaction, with last, the last message id.
func storeEntries(db *bbolt.DB, es []entry, last int64) error {
	return db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(historyBucket)
		for _, e := range es {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			if err := b.Put(idKey(int64(seq)), []byte(puff.Print(e.stored()))); err != nil {
				return err
			}
		}
		return storeInt(tx, msgsKey, last)
	})
}

// loadHistory reads the last historySize entries of tx's history into st,
// and the last message id. One it can't read is left out, and noted.
func (st *state) loadHistory(tx *bbolt.Tx) error {
	c := tx.Bucket(historyBucket).Cursor()
	bad := 0
	for k, v := c.Last(); k != nil && len(st.history) < historySize; k, v = c.Prev() {
		e, err := storedEntry(string(v))
		if err != nil {
			bad++
			continue
		}
		st.history = append(st.history, e)
		st.lastMsg = max(st.lastMsg, e.id)
	}
	slices.Reverse(st.history)
	n, err := storedInt(tx, msgsKey)
	st.lastMsg = max(st.lastMsg, n)
	if bad > 0 {
		st.note(fmt.Sprintf("%d history entries can't be read, and stay stored", bad))
	}
	return err
}

// pruneHistory deletes the entries of tx's history older than cutoff:
// the first ones, as they are in the order they were recorded. One it
// can't read stays, as its age is unknown.
func pruneHistory(tx *bbolt.Tx, cutoff time.Time) error {
	b := tx.Bucket(historyBucket)
	var old [][]byte
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		e, err := storedEntry(string(v))
		if err != nil {
			continue
		}
		if !e.at.Before(cutoff) {
			break
		}
		old = append(old, slices.Clone(k))
	}
	for _, k := range old {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// note records something about whiff itself in history, and in the log.
func (d *daemon) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(d.log, "whiff: %s\n", msg)
	d.do(func(st *state) { st.record(entry{at: time.Now(), note: msg}) })
}

// note is d.note on the route goroutine, which can't wait for itself.
func (st *state) note(msg string) {
	fmt.Fprintf(st.log, "whiff: %s\n", msg)
	st.record(entry{at: time.Now(), note: msg})
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
