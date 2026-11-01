package daemon

import (
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// storeTimeout is how long openStore waits for a daemon that is stopping
// to close whiff.db: until its services finish the messages they handle.
const storeTimeout = 10 * time.Second

// stateBucket holds each stateful service's last committed state, printed,
// by name.
var stateBucket = []byte("state")

// metaBucket holds nextIDKey, the last id given out, and agentsKey, the last
// agent's number, so each means one thing even across a restart: a stale
// whiff reply can't answer another message.
var (
	metaBucket = []byte("meta")
	nextIDKey  = []byte("nextID")
	agentsKey  = []byte("agents")
)

// inboxBucket holds what waits in every durable inbox, printed, by id, so
// it reads back oldest first.
var inboxBucket = []byte("inbox")

// timersBucket holds each send-after waiting for its time, printed, by when
// it is due.
var timersBucket = []byte("timers")

// openStore opens whiff.db, next to the services directory (so
// ~/.whiff/whiff.db), with its buckets. Only the daemon opens it.
func openStore(services string) (*bbolt.DB, error) {
	dir := filepath.Dir(services)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(filepath.Join(dir, "whiff.db"), 0o600, &bbolt.Options{Timeout: storeTimeout})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{stateBucket, metaBucket, inboxBucket, timersBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// storedState is name's committed state, printed, if it has one.
func (d *daemon) storedState(name string) (p string, found bool, err error) {
	err = d.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(stateBucket).Get([]byte(name)); v != nil {
			p, found = string(v), true
		}
		return nil
	})
	return p, found, err
}

// stateOf is (ok state) for the service name, as it last committed it, read
// from whiff.db, not from the service, which may be handling a message.
func (d *daemon) stateOf(name string) (puff.Value, error) {
	var s *service
	d.do(func(st *state) { s = st.addrs[name] })
	switch {
	case s == nil || s.client:
		return puff.Errorf("no-address", "no service :%s", name), nil
	case !s.stateful:
		return puff.Errorf("stateless", ":%s has no state", name), nil
	case !s.keep:
		return puff.Errorf("stateless", ":%s is a trial, whose state isn't kept", name), nil
	}
	p, found, err := d.storedState(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return puff.Errorf("stateless", ":%s has no state stored", name), nil
	}
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, fmt.Errorf(":%s's stored state: %v", name, err)
	}
	return puff.Ok(v), nil
}

// storeState commits p as name's state.
func (d *daemon) storeState(name, p string) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stateBucket).Put([]byte(name), []byte(p))
	})
}

// forgetState deletes name's committed state, if any.
func (d *daemon) forgetState(name string) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stateBucket).Delete([]byte(name))
	})
}

// idKey is id as a key that sorts by id.
func idKey(id int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(id))
}

// storedInt is the number under key in tx's meta bucket; 0 if there is none.
func storedInt(tx *bbolt.Tx, key []byte) (int64, error) {
	v := tx.Bucket(metaBucket).Get(key)
	if v == nil {
		return 0, nil
	}
	return strconv.ParseInt(string(v), 10, 64)
}

// storeInt raises the number under key in tx's meta bucket to n: never
// lowers it, as connections take numbers side by side.
func storeInt(tx *bbolt.Tx, key []byte, n int64) error {
	old, err := storedInt(tx, key)
	if err != nil || old >= n {
		return err
	}
	return tx.Bucket(metaBucket).Put(key, strconv.AppendInt(nil, n, 10))
}

// newID is a new id for a message answered later, or a job. One that can't
// be stored still works until the next restart, so it is only noted.
func (st *state) newID() int64 {
	st.nextID++
	err := st.db.Update(func(tx *bbolt.Tx) error { return storeInt(tx, nextIDKey, st.nextID) })
	if err != nil {
		st.note(fmt.Sprintf("the last id, %d, isn't stored: %v", st.nextID, err))
	}
	return st.nextID
}

// newAgent is the number of a new agent session. A number names one session
// for good, so a held message's sender can't turn out to be another's, nor
// Trust reach a session you never saw.
func (d *daemon) newAgent() int64 {
	n := d.agents.Add(1)
	err := d.db.Update(func(tx *bbolt.Tx) error { return storeInt(tx, agentsKey, n) })
	if err != nil {
		d.note("the last agent's number, %d, isn't stored: %v", n, err)
	}
	return n
}

// durable says whether a's inbox survives a restart: every one but those of
// whiff try's scratch addresses, which go when it does.
func (a *asked) durable() bool {
	return !strings.HasPrefix(a.addr, "try/")
}

// keep stores a as it is now, if its inbox is durable.
func (st *state) keep(a *asked) error {
	if !a.durable() {
		return nil
	}
	return st.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(inboxBucket).Put(idKey(a.id), []byte(puff.Print(a.stored())))
	})
}

// forget deletes a, taken from its inbox. One that can't be deleted comes
// back after a restart, so it is only noted.
func (st *state) forget(a *asked) {
	if !a.durable() {
		return
	}
	err := st.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(inboxBucket).Delete(idKey(a.id))
	})
	if err != nil {
		st.note(fmt.Sprintf("message %d stays stored in :%s's inbox: %v", a.id, a.addr, err))
	}
}

// load reads what was stored into a new state: the inboxes, the timers, and
// the last id, which is at least the last id in the inboxes. What it can't
// read stays stored, and noted, rather than being lost.
func (st *state) load() {
	err := st.db.View(func(tx *bbolt.Tx) error {
		err := tx.Bucket(inboxBucket).ForEach(func(k, v []byte) error {
			id := int64(binary.BigEndian.Uint64(k))
			st.nextID = max(st.nextID, id)
			a, err := storedAsked(id, string(v))
			if err != nil {
				st.note(fmt.Sprintf("message %d in an inbox can't be read, and stays stored: %v", id, err))
				return nil
			}
			st.asked = append(st.asked, a)
			return nil
		})
		if err != nil {
			return err
		}
		err = tx.Bucket(timersBucket).ForEach(func(k, v []byte) error {
			t, err := storedTimer(slices.Clone(k), string(v))
			if err != nil {
				st.note(fmt.Sprintf("a timer can't be read, and stays stored: %v", err))
				return nil
			}
			st.timers = append(st.timers, t)
			return nil
		})
		if err != nil {
			return err
		}
		n, err := storedInt(tx, nextIDKey)
		st.nextID = max(st.nextID, n)
		return err
	})
	if err != nil {
		st.note(fmt.Sprintf("whiff.db: %v", err))
	}
}

// resume takes up what load read, once the services are loaded so that an
// answer finds its sender: asks with :expires wait out what is left of it,
// or end now if it passed while whiff was down, and so do timers. What waits
// for an address that isn't loaded stays, for a client to claim it or a file
// to be fixed; :jobs, :me and :review are always there. A service with a
// :start drops the timers it set itself, as a reload does: its :start has
// just set its own, so its loops don't double.
func (d *daemon) resume() {
	d.do(func(st *state) {
		away := map[string]int{}
		for _, a := range st.asked {
			if a.expires > 0 {
				d.expireAfter(a, time.Until(a.at.Add(a.expires)))
			}
			if st.addrs[a.addr] == nil {
				away[a.addr]++
			}
		}
		for _, addr := range slices.Sorted(maps.Keys(away)) {
			st.note(fmt.Sprintf("%d waiting for :%s, which isn't loaded", away[addr], addr))
		}
		dropped := map[string]int{}
		for _, t := range st.timers {
			var stop <-chan struct{}
			if s := st.addrs[t.addr]; t.own() && s != nil {
				if s.starts {
					dropped[t.addr]++
					d.forgetTimer(t)
					continue
				}
				stop = s.quit
			}
			d.arm(t, stop)
		}
		for _, addr := range slices.Sorted(maps.Keys(dropped)) {
			st.note(fmt.Sprintf(":%s's :start set its timers again, so whiff dropped %d stored", addr, dropped[addr]))
		}
		st.timers = nil
	})
}

// timer is a send-after waiting for its time: m, for addr, built when it was
// set, so it keeps that origin.
type timer struct {
	key  []byte // where it is stored; nil for one that isn't
	due  time.Time
	addr string
	m    message
}

// own says whether t is a service's timer to itself.
func (t *timer) own() bool { return t.addr == t.m.from }

// durable says whether t survives a restart: not one to or from whiff try's
// scratch addresses, which go when it does.
func (t *timer) durable() bool {
	return !strings.HasPrefix(t.addr, "try/") && !strings.HasPrefix(t.m.from, "try/")
}

// keepTimer stores t. Its key is when it is due, then a number that makes it
// its own: timers can share a due time.
func (d *daemon) keepTimer(t *timer) error {
	if !t.durable() {
		return nil
	}
	key := append(idKey(t.due.UnixNano()), idKey(d.timers.Add(1))...)
	err := d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(timersBucket).Put(key, []byte(puff.Print(t.stored())))
	})
	if err == nil {
		t.key = key
	}
	return err
}

// forgetTimer deletes t once it is done with. One that can't be deleted
// fires again after a restart, so it is noted.
func (d *daemon) forgetTimer(t *timer) {
	if t.key == nil {
		return
	}
	err := d.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(timersBucket).Delete(t.key) })
	if err != nil {
		fmt.Fprintf(d.log, "whiff: a timer for :%s stays stored: %v\n", t.m.to(t.addr), err)
	}
}

// arm delivers t when it is due, once: it leaves the store first, so a
// restart can't send it twice. It goes unsent if stop closes first, the
// version of the service that set it having gone, and stays stored if whiff
// is stopping. A delivery that fails is in history.
func (d *daemon) arm(t *timer, stop <-chan struct{}) {
	time.AfterFunc(time.Until(t.due), func() {
		if d.ctx.Err() != nil {
			return
		}
		d.forgetTimer(t)
		select {
		case <-stop:
			return
		default:
		}
		d.deliver(t.addr, t.m)
	})
}

// stored is t as it is stored.
func (t *timer) stored() puff.Value {
	args, _ := puff.ReadValue(t.m.args)
	es := []puff.Entry{
		{Key: puff.Keyword("due"), Val: puff.NewTime(t.due)},
		{Key: puff.Keyword("to"), Val: puff.Keyword(t.addr)},
		{Key: puff.Keyword("fn"), Val: puff.Keyword(t.m.fn)},
		{Key: puff.Keyword("args"), Val: args},
		{Key: puff.Keyword("origin"), Val: origin(t.m.user)},
		{Key: puff.Keyword("reviewed"), Val: puff.Bool(t.m.reviewed)},
		{Key: puff.Keyword("cwd"), Val: puff.String(t.m.dir)},
	}
	if t.m.from != "" {
		es = append(es, puff.Entry{Key: puff.Keyword("from"), Val: puff.Keyword(t.m.from)})
	}
	if t.m.session != "" {
		es = append(es, puff.Entry{Key: puff.Keyword("session"), Val: puff.Keyword(t.m.session)})
	}
	m, _ := puff.NewMap(es...)
	return m
}

// storedTimer is the timer stored under key, from p, as stored printed it.
// As for a message, only a stored :user is the user's, and only a stored
// #t has been reviewed.
func storedTimer(key []byte, p string) (*timer, error) {
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, err
	}
	m, ok := v.(puff.Map)
	if !ok {
		return nil, fmt.Errorf("want a map, got %s", p)
	}
	get := func(k string) puff.Value {
		v, _ := m.Get(puff.Keyword(k))
		return v
	}
	name := func(k string) string {
		s, _ := get(k).(puff.Keyword)
		return string(s)
	}
	to, fn := name("to"), name("fn")
	args, ok := get("args").(puff.Vector)
	due, isTime := get("due").(puff.Time)
	if to == "" || fn == "" || !ok || !isTime {
		return nil, fmt.Errorf("want :to, :fn, :args and :due, got %s", p)
	}
	dir, _ := get("cwd").(puff.String)
	reviewed, _ := get("reviewed").(puff.Bool)
	return &timer{key: key, due: due.Go(), addr: to, m: message{from: name("from"), fn: fn, args: puff.Print(args), dir: string(dir),
		user: name("origin") == string(origin(true)), reviewed: bool(reviewed), session: name("session")}}, nil
}

// stored is a as it is stored: as (inbox) shows it, with what a restart
// needs too. Its cause isn't there: message ids begin again with each
// daemon, so it could name another's.
func (a *asked) stored() puff.Value {
	v := a.value().(puff.Map)
	with := func(key string, val puff.Value) {
		v, _ = v.With(puff.Keyword(key), val)
	}
	with("addr", puff.Keyword(a.addr))
	if a.session != "" {
		with("session", puff.Keyword(a.session))
	}
	if a.expires > 0 {
		with("expires", puff.Duration(a.expires))
	}
	switch {
	case a.addr != "review":
	case a.held == nil:
		with("held", puff.Keyword("eval"))
	case a.held.topic != "":
		with("held", puff.Keyword("publish"))
	default:
		with("held", puff.Keyword("message"))
	}
	return v
}

// storedAsked is the message id from p, as keep printed it. Only a stored
// :user is the user's, so nothing here turns an agent's message into yours,
// and a message for :review comes back as it was held: accepting it delivers
// what you see.
func storedAsked(id int64, p string) (*asked, error) {
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, err
	}
	m, ok := v.(puff.Map)
	if !ok {
		return nil, fmt.Errorf("want a map, got %s", p)
	}
	key := func(k string) string {
		v, _ := m.Get(puff.Keyword(k))
		s, _ := v.(puff.Keyword)
		return string(s)
	}
	addr, fn := key("addr"), key("fn")
	if addr == "" || fn == "" {
		return nil, fmt.Errorf("want :addr and :fn, got %s", p)
	}
	get := func(k string) puff.Value {
		v, _ := m.Get(puff.Keyword(k))
		return v
	}
	args, ok := get("args").(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want :args as a vector, got %s", p)
	}
	dir, _ := get("cwd").(puff.String)
	at, _ := get("at").(puff.Time)
	expires, _ := get("expires").(puff.Duration)
	a := &asked{ticket: ticket{id: id, from: key("from"), dir: string(dir), user: key("origin") == string(origin(true)), session: key("session")},
		addr: addr, fn: fn, args: args, at: at.Go(), expires: time.Duration(expires)}
	switch addr {
	case "me":
		err = a.checkMe()
	case "review":
		err = a.readHeld(key("held"), key("to"))
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// checkMe says what is wrong with a, read for :me's inbox: meItem counts on
// its being what :me/ask and :me/tell check.
func (a *asked) checkMe() error {
	if a.args.Len() < 1 || a.args.Len() > 2 {
		return fmt.Errorf(":me/%s takes 1 to 2 arguments, got %d", a.fn, a.args.Len())
	}
	switch a.fn {
	case "ask":
		return askOpts(a.args)
	case "tell":
		return tellOpts(a.args)
	}
	return fmt.Errorf("no :me/%s", a.fn)
}

// readHeld sets what a, read for :review's inbox, holds: an eval, a publish
// to the topic to, or a message to to.
func (a *asked) readHeld(kind, to string) error {
	if kind == "eval" {
		var src puff.String
		if a.args.Len() == 1 {
			src, _ = a.args.At(0).(puff.String)
		}
		if src == "" {
			return fmt.Errorf("want an eval's code, got %s", puff.Print(a.args))
		}
		a.src = string(src)
		return nil
	}
	if to == "" {
		return fmt.Errorf("want :to for a held %s", kind)
	}
	held := message{from: a.from, args: puff.Print(a.args), dir: a.dir, user: a.user, session: a.session}
	switch kind {
	case "publish":
		held.topic = to
	case "message":
		held.fn = a.fn
	default:
		return fmt.Errorf("want :held :eval, :publish or :message, got %q", kind)
	}
	a.held, a.to = &held, to
	return nil
}
