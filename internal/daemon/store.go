package daemon

import (
	"context"
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
	"github.com/synthlike/whiff/internal/sandbox"
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

// tasksBucket holds every task, printed, by id: a job as it runs, then each
// task's result from when it ends until resultKept has passed. One found
// running at start was running when whiff went: see resume.
var tasksBucket = []byte("tasks")

// resultKept is how long a task's result is kept after it ended.
const resultKept = 30 * 24 * time.Hour

// openStore opens whiff.db, next to the services directory (so
// ~/.whiff/whiff.db), with its buckets. Only the daemon opens it. It
// deletes history older than historyKept, then compacts whiff.db if that
// left much of it free.
func openStore(services string) (*bbolt.DB, error) {
	dir := filepath.Dir(services)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "whiff.db")
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: storeTimeout})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{stateBucket, metaBucket, inboxBucket, timersBucket, tasksBucket, historyBucket, modesBucket, peersBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return pruneHistory(tx, time.Now().Add(-historyKept))
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return compactStore(db)
}

// compactStore is db, compacted into a new file that takes its place, if a
// quarter of it or more is free: bbolt reuses free pages but never gives
// them back. If compacting fails, it is db as it was, and the log says so.
func compactStore(db *bbolt.DB) (*bbolt.DB, error) {
	path := db.Path()
	fi, err := os.Stat(path)
	if err != nil {
		return db, nil
	}
	s := db.Stats()
	free := int64(s.FreePageN+s.PendingPageN) * int64(db.Info().PageSize)
	if fi.Size() < 1<<20 || free*4 < fi.Size() {
		return db, nil
	}
	tmp := path + ".compact"
	os.Remove(tmp)
	dst, err := bbolt.Open(tmp, 0o600, nil)
	if err == nil {
		err = bbolt.Compact(dst, db, 64<<20)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "whiff: whiff.db wasn't compacted: %v\n", err)
		return db, nil
	}
	if err := db.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "whiff: whiff.db wasn't compacted: %v\n", err)
	}
	return bbolt.Open(path, 0o600, &bbolt.Options{Timeout: storeTimeout})
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

// kept is where a message stays stored until its service has handled it, so
// a restart gives it again: the result of a task, or the timer of a
// send-after. The zero kept is a message that isn't stored.
type kept struct {
	task  int64  // the task whose result a whiff/done is
	timer string // the key of the stored timer that sent it
}

// storeState commits p as name's state, and in the same transaction marks
// k, the message name is handling, handled: so a restart gives again only a
// message whose effect on the state was lost.
func (d *daemon) storeState(name, p string, k kept) error {
	err := d.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(stateBucket).Put([]byte(name), []byte(p)); err != nil {
			return err
		}
		return k.handled(tx, name)
	})
	if err == nil && k.timer != "" {
		d.timersChanged()
	}
	return err
}

// handled marks k handled by owner, which isn't given it again. One that
// can't be marked comes again after a restart, so it is noted.
func (d *daemon) handled(owner string, k kept) {
	err := d.db.Update(func(tx *bbolt.Tx) error { return k.handled(tx, owner) })
	switch {
	case err == nil && k.timer != "":
		d.timersChanged()
	case err == nil:
	case k.task != 0:
		fmt.Fprintf(d.log, "whiff: the result of task %d may come to :%s again: %v\n", k.task, owner, err)
	default:
		fmt.Fprintf(d.log, "whiff: a timer for :%s may fire again: %v\n", owner, err)
	}
}

// handled marks k handled by owner, in tx: its task's result, and its
// timer, which is deleted.
func (k kept) handled(tx *bbolt.Tx, owner string) error {
	if k.task != 0 {
		if err := markHandled(tx, k.task, owner); err != nil {
			return err
		}
	}
	if k.timer == "" {
		return nil
	}
	return tx.Bucket(timersBucket).Delete([]byte(k.timer))
}

// markHandled marks the result of task id handled by owner, in tx, unless
// the task is another's now: one handed on after it ended is its new
// owner's to handle.
func markHandled(tx *bbolt.Tx, id int64, owner string) error {
	b := tx.Bucket(tasksBucket)
	p := b.Get(idKey(id))
	if p == nil {
		return nil
	}
	v, err := puff.ReadValue(string(p))
	if err != nil {
		return err
	}
	m, _ := v.(puff.Map)
	from := puff.Field(m, "from")
	if f, _ := from.(puff.Keyword); string(f) != owner {
		return nil
	}
	if m, err = m.With(puff.Keyword("handled"), puff.Bool(true)); err != nil {
		return err
	}
	return b.Put(idKey(id), []byte(puff.Print(m)))
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

// acceptHeld takes the message id out of :review's inbox and stores it as
// an accepted task, in one transaction: so if whiff dies before it ends,
// its sender hears :interrupted :unknown, rather than nothing, and it isn't
// held again. nil if it isn't there; the error leaves it held.
func (st *state) acceptHeld(id int64) (*asked, error) {
	i := slices.IndexFunc(st.asked, func(a *asked) bool { return a.id == id && a.addr == "review" })
	if i < 0 {
		return nil, nil
	}
	a := st.asked[i]
	m, err := puff.NewMap(append(ticketEntries(a.ticket),
		puff.Key("status", puff.Keyword("accepted")))...)
	if err != nil {
		return nil, err
	}
	err = st.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(inboxBucket).Delete(idKey(id)); err != nil {
			return err
		}
		return tx.Bucket(tasksBucket).Put(idKey(id), []byte(puff.Print(m)))
	})
	if err != nil {
		return nil, err
	}
	st.asked = slices.Delete(st.asked, i, i+1)
	return a, nil
}

// load reads what was stored into a new state: the end of history, the
// inboxes, the timers, the jobs whiff lost, agents' sessions' modes, and the last id, which is at
// least the last id in the inboxes and tasks. What it can't read stays
// stored, and noted, rather than being lost. It deletes the results that
// ended more than resultKept ago.
func (st *state) load() {
	err := st.db.Update(func(tx *bbolt.Tx) error {
		// First, so what load notes comes after it.
		if err := st.loadHistory(tx); err != nil {
			return err
		}
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
			// k lasts only as long as the transaction.
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
		tasks := tx.Bucket(tasksBucket)
		cutoff := time.Now().Add(-resultKept)
		var old [][]byte
		err = tasks.ForEach(func(k, v []byte) error {
			id := int64(binary.BigEndian.Uint64(k))
			st.nextID = max(st.nextID, id)
			get, err := fields(string(v))
			var status puff.Keyword
			if err == nil {
				status, _ = get("status").(puff.Keyword)
			}
			if err == nil && status == "accepted" {
				st.cut = append(st.cut, storedTicket(id, get))
				return nil
			}
			if err == nil && status != "ended" {
				var j *job
				if j, err = storedJob(id, string(v)); err == nil {
					st.lost = append(st.lost, j)
					return nil
				}
			}
			if err != nil {
				st.note(fmt.Sprintf("task %d can't be read, and stays stored: %v", id, err))
				return nil
			}
			// Only a stored #f is a result its owner has yet to handle.
			handled, ok := get("handled").(puff.Bool)
			ended, _ := get("ended").(puff.Time)
			switch by, _ := get("by").(puff.Keyword); {
			case ok && !bool(handled) && get("result") != nil:
				st.owed = append(st.owed, answered{storedTicket(id, get), string(by), get("result")})
			case ended.Go().Before(cutoff):
				old = append(old, slices.Clone(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range old {
			if err := tasks.Delete(k); err != nil {
				return err
			}
		}
		err = tx.Bucket(modesBucket).ForEach(func(k, v []byte) error {
			m, err := storedMode(string(v))
			if err != nil {
				st.note(fmt.Sprintf(":%s's mode can't be read, so it is gated: %v", k, err))
				return nil
			}
			st.modes[string(k)] = m
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
// just set its own, so its loops don't double. A job that was running runs
// again if it may: see again. The sender of each of the others hears how it
// ended, as far as whiff knows. A result its owner hadn't handled went to
// it as it loaded; one for an owner that won't handle it, a client or a
// service with no file, is a line in history.
func (d *daemon) resume() {
	type rerun struct {
		j *job
		r puff.Value
	}
	var again []rerun
	defer func() {
		for _, a := range again {
			d.rerun(a.j, a.r)
		}
	}()
	d.endStrays()
	d.do(func(st *state) {
		away := map[string]int{}
		owed := st.owed
		st.owed = nil
		for _, a := range owed {
			if !d.tell(st, a.ticket, a.by, a.result) {
				d.handled(a.from, kept{task: a.id})
			} else if st.addrs[a.from] == nil {
				away[a.from]++
			}
		}
		for _, j := range st.lost {
			r := lost(j)
			// Not while any of it may still run: it would run twice at once.
			if j.again(r) && !j.mayRun() {
				again = append(again, rerun{j, r})
				continue
			}
			d.answer(st, j.ticket, "jobs", r)
			st.publish(jobDone(j, r))
		}
		st.lost = nil
		for _, t := range st.cut {
			d.answer(st, t, "review", failed("interrupted", "unknown", "task %d was accepted as whiff died: whether it happened is unknown", t.id))
		}
		st.cut = nil
		for _, a := range st.asked {
			if a.expires > 0 {
				d.expireAfter(a, time.Until(a.at.Add(a.expires)))
			}
			if st.addrs[a.addr] == nil {
				away[a.addr]++
			}
		}
		// An agent whose file has gone takes its session's mode with it, as
		// it would have had whiff been running; one that fails to load
		// keeps it, as it does then.
		for _, addr := range slices.Sorted(maps.Keys(st.modes)) {
			name := strings.TrimPrefix(addr, "agent/")
			if agentSession(addr) && st.addrs[name] == nil && st.health[name] == nil {
				st.setMode(addr, nil)
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
			d.arm(t, stop, nil)
		}
		for _, addr := range slices.Sorted(maps.Keys(dropped)) {
			st.note(fmt.Sprintf(":%s's :start set its timers again, so whiff dropped %d stored", addr, dropped[addr]))
		}
		st.timers = nil
	})
}

// endStrays ends the containers of sandboxed jobs that were running when
// whiff died. A container's name is its own, as a reused pid isn't, so
// whiff can end what's left of one; then its client's group goes too, and
// the job is lost as one that runs no more: its outcome is unknown, and it
// may run again.
func (d *daemon) endStrays() {
	var strays []*job
	d.do(func(st *state) {
		for _, j := range st.lost {
			if n := len(j.attempts); j.box != "" && n > 0 && j.attempts[n-1].ended.IsZero() {
				strays = append(strays, j)
			}
		}
	})
	if len(strays) == 0 {
		return
	}
	c, err := sandbox.Host()
	if err != nil {
		d.note("%d sandboxed jobs were running when whiff died, and their containers can't be checked: %v", len(strays), err)
		return
	}
	for _, j := range strays {
		ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		if !c.Gone(ctx, j.box) {
			c.End(ctx, j.box)
			d.note("job %d's container %s still ran after whiff died, so whiff ended it", j.id, j.box)
		}
		cancel()
		for deadline := time.Now().Add(stopGrace); groupThere(j.pid) && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
	}
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

// keepJob stores j, running, as it is now.
func (d *daemon) keepJob(j *job) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(tasksBucket).Put(idKey(j.id), []byte(puff.Print(j.stored())))
	})
}

// endKeys are what endTask writes of a task, over what was stored of it.
var endKeys = []string{"id", "from", "origin", "cwd", "session", "reviewed", "what", "parent", "project", "status", "ended", "by", "result", "handled", "stdin"}

// endTask stores r as the result of task t, answered by by, over what was
// stored of it as it ran, such as a job's :cmd. handled is whether t's
// owner is done with it once told: not a service that handles whiff/done,
// which marks it as it does, nor anyone if whiff is stopping. The owner is
// t's, so a task handed on is stored as the new owner's. An ask or held
// message leaves its stored inbox in the same transaction, so whiff can't
// die between the two and lose the answer.
func endTask(db *bbolt.DB, t ticket, by string, r puff.Value, handled bool) error {
	return db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(inboxBucket).Delete(idKey(t.id)); err != nil {
			return err
		}
		b := tx.Bucket(tasksBucket)
		var es []puff.Entry
		if p := b.Get(idKey(t.id)); p != nil {
			if v, err := puff.ReadValue(string(p)); err == nil {
				was, _ := v.(puff.Map)
				for i := range was.Len() {
					if e := was.Entry(i); !slices.ContainsFunc(endKeys, func(k string) bool { return puff.Equal(e.Key, puff.Keyword(k)) }) {
						es = append(es, e)
					}
				}
			}
		}
		es = append(es, ticketEntries(t)...)
		es = append(es,
			puff.Key("status", puff.Keyword("ended")),
			puff.Key("ended", puff.NewTime(time.Now())),
		)
		if by != "" {
			es = append(es, puff.Key("by", puff.Keyword(by)))
		}
		es = append(es,
			puff.Key("result", r),
			puff.Key("handled", puff.Bool(handled)),
		)
		m, err := puff.NewMap(es...)
		if err != nil {
			return err
		}
		return b.Put(idKey(t.id), []byte(puff.Print(m)))
	})
}

// storedResult is the owner of task id, who answered it and its result, if
// it has ended and is still kept; r is nil if it hasn't, or isn't.
func storedResult(db *bbolt.DB, id int64) (t ticket, by string, r puff.Value, err error) {
	err = db.View(func(tx *bbolt.Tx) error {
		p := tx.Bucket(tasksBucket).Get(idKey(id))
		if p == nil {
			return nil
		}
		get, err := fields(string(p))
		if err != nil {
			return fmt.Errorf("task %d can't be read: %v", id, err)
		}
		if status, _ := get("status").(puff.Keyword); status != "ended" {
			return nil
		}
		b, _ := get("by").(puff.Keyword)
		t, by, r = storedTicket(id, get), string(b), get("result")
		return nil
	})
	return t, by, r, err
}

// ticketEntries is t as a task stores it: {:id :from :origin :cwd :session
// :reviewed}, without :from for a client, :session outside one nor
// :reviewed unless you gave the answer.
func ticketEntries(t ticket) []puff.Entry {
	es := []puff.Entry{puff.Key("id", puff.Int(t.id))}
	if t.from != "" {
		es = append(es, puff.Key("from", puff.Keyword(t.from)))
	}
	es = append(es,
		puff.Key("origin", origin(t.user)),
		puff.Key("cwd", puff.String(t.dir)),
	)
	if t.session != "" {
		es = append(es, puff.Key("session", puff.Keyword(t.session)))
	}
	if t.reviewed {
		es = append(es, puff.Key("reviewed", puff.Bool(true)))
	}
	if t.about != "" {
		es = append(es, puff.Key("what", puff.String(t.about)))
	}
	if t.task != 0 {
		es = append(es, puff.Key("parent", puff.Int(t.task)))
	}
	if t.project != "" {
		es = append(es, puff.Key("project", puff.String(t.project)))
	}
	return es
}

// storedTicket is the ticket of task id, from get, its stored fields. Only
// a stored :user is the user's, and only a stored #t was reviewed. As for
// a message, its cause isn't there.
func storedTicket(id int64, get func(string) puff.Value) ticket {
	from, _ := get("from").(puff.Keyword)
	org, _ := get("origin").(puff.Keyword)
	session, _ := get("session").(puff.Keyword)
	dir, _ := get("cwd").(puff.String)
	reviewed, _ := get("reviewed").(puff.Bool)
	about, _ := get("what").(puff.String)
	parent, _ := get("parent").(puff.Int)
	project, _ := get("project").(puff.String)
	return ticket{id: id, from: string(from), dir: string(dir), about: string(about), lineage: lineage{user: org == origin(true), session: string(session), reviewed: bool(reviewed), task: int64(parent), project: string(project)}}
}

// fields is the printed map p as a lookup: nil for a key it hasn't.
func fields(p string) (func(string) puff.Value, error) {
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, err
	}
	m, ok := v.(puff.Map)
	if !ok {
		return nil, fmt.Errorf("want a map, got %s", p)
	}
	return func(k string) puff.Value { return puff.Field(m, k) }, nil
}

// stored is j as it is stored while it runs: as :jobs/list shows it, with
// what a restart needs too: its attempts, and how it runs if it may run
// again.
func (j *job) stored() puff.Value {
	v := j.value().(puff.Map)
	with := func(key string, val puff.Value) {
		v, _ = v.With(puff.Keyword(key), val)
	}
	with("pid", puff.Int(j.pid))
	with("origin", origin(j.user))
	if j.session != "" {
		with("session", puff.Keyword(j.session))
	}
	if j.reviewed {
		with("reviewed", puff.Bool(true))
	}
	if j.project != "" {
		with("project", puff.String(j.project))
	}
	with("status", puff.Keyword("running"))
	var as []puff.Value
	for _, a := range j.attempts {
		es := []puff.Entry{
			puff.Key("started", puff.NewTime(a.started)),
			puff.Key("pid", puff.Int(a.pid)),
		}
		if a.box != "" {
			es = append(es, puff.Key("container", puff.String(a.box)))
		}
		if !a.ended.IsZero() {
			es = append(es,
				puff.Key("ended", puff.NewTime(a.ended)),
				puff.Key("outcome", puff.Keyword(a.outcome)))
		}
		m, _ := puff.NewMap(es...)
		as = append(as, m)
	}
	with("attempts", puff.NewVector(as...))
	if o := j.opts; o.retry > 0 {
		with("retry", puff.Int(o.retry))
		if o.timeout > 0 {
			with("timeout", puff.Duration(o.timeout))
		} else {
			with("timeout", puff.Keyword("none"))
		}
		with("dir", puff.String(o.dir))
		if o.stdin != nil {
			with("stdin", puff.String(*o.stdin))
		}
		if o.text {
			with("as", puff.Keyword("text"))
		}
		if o.idempotent {
			with("idempotent", puff.Bool(true))
		}
		if o.stream {
			with("stream", puff.Bool(true))
		}
		if o.sandbox {
			sb, _ := puff.NewMap(puff.Key("net", puff.Bool(o.net)))
			with("sandbox", sb)
		}
	}
	return v
}

// storedJob is the job id from p, as stored printed it as it ran.
func storedJob(id int64, p string) (*job, error) {
	get, err := fields(p)
	if err != nil {
		return nil, err
	}
	cmd, ok := get("cmd").(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want :cmd as a vector, got %s", p)
	}
	pid, _ := get("pid").(puff.Int)
	box, _ := get("container").(puff.String)
	j := &job{ticket: storedTicket(id, get), cmd: cmd, pid: int(pid), box: string(box), opts: jobOpts{timeout: defaultJobTimeout}}
	if sb, ok := get("sandbox").(puff.Map); ok {
		net, _ := puff.Field(sb, "net").(puff.Bool)
		j.opts.sandbox, j.opts.net = true, bool(net)
	}
	j.opts.dir = j.dir
	if t, ok := get("timeout").(puff.Duration); ok && t > 0 {
		j.opts.timeout = time.Duration(t)
	} else if puff.Equal(get("timeout"), puff.Keyword("none")) {
		j.opts.timeout = 0
	}
	if s, ok := get("dir").(puff.String); ok {
		j.opts.dir = string(s)
	}
	if s, ok := get("stdin").(puff.String); ok {
		j.opts.stdin = (*string)(&s)
	}
	n, _ := get("retry").(puff.Int)
	idem, _ := get("idempotent").(puff.Bool)
	stream, _ := get("stream").(puff.Bool)
	j.opts.retry, j.opts.text, j.opts.idempotent, j.opts.stream = int(n), puff.Equal(get("as"), puff.Keyword("text")), bool(idem), bool(stream)
	as, _ := get("attempts").(puff.Vector)
	for i := range as.Len() {
		m, _ := as.At(i).(puff.Map)
		at := func(k string) puff.Value { return puff.Field(m, k) }
		started, _ := at("started").(puff.Time)
		ended, isEnded := at("ended").(puff.Time)
		outcome, _ := at("outcome").(puff.Keyword)
		apid, _ := at("pid").(puff.Int)
		abox, _ := at("container").(puff.String)
		a := attempt{started: started.Go(), outcome: string(outcome), pid: int(apid), box: string(abox)}
		if isEnded {
			a.ended = ended.Go()
		}
		j.attempts = append(j.attempts, a)
	}
	return j, nil
}

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
		d.timersChanged()
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
	d.timersChanged()
}

// arm delivers t when it is due. In a mailbox, its message stays stored
// until the service has handled it, with its new state if it has one, so if
// whiff dies first, it fires again at start; anything else ends its delivery
// at once, as an inbox stores it, or history has why it failed. It goes
// unsent if stop closes first, the version of the service that set it
// having gone, and stays stored if whiff is stopping. One due before
// placed closes, as a :start's can be while its service loads, waits for
// it: the service has its address only then.
func (d *daemon) arm(t *timer, stop, placed <-chan struct{}) {
	time.AfterFunc(time.Until(t.due), func() {
		if d.ctx.Err() != nil {
			return
		}
		select {
		case <-stop:
			d.forgetTimer(t)
			return
		default:
		}
		if placed != nil {
			select {
			case <-placed:
			case <-stop:
				d.forgetTimer(t)
				return
			case <-d.ctx.Done():
				return
			}
		}
		// Cancelled, it is no longer stored.
		if !d.stillSet(t) {
			return
		}
		m := t.m
		m.kept.timer = string(t.key)
		if d.deliver(t.addr, m) != nil && d.ctx.Err() == nil {
			d.forgetTimer(t)
		}
	})
}

// stored is t as it is stored.
func (t *timer) stored() puff.Value {
	args, _ := puff.ReadValue(t.m.args)
	es := []puff.Entry{
		puff.Key("due", puff.NewTime(t.due)),
		puff.Key("to", puff.Keyword(t.addr)),
		puff.Key("fn", puff.Keyword(t.m.fn)),
		puff.Key("args", args),
		puff.Key("origin", origin(t.m.user)),
		puff.Key("reviewed", puff.Bool(t.m.reviewed)),
		puff.Key("cwd", puff.String(t.m.dir)),
	}
	if t.m.from != "" {
		es = append(es, puff.Key("from", puff.Keyword(t.m.from)))
	}
	if t.m.session != "" {
		es = append(es, puff.Key("session", puff.Keyword(t.m.session)))
	}
	if t.m.project != "" {
		es = append(es, puff.Key("project", puff.String(t.m.project)))
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
	get := func(k string) puff.Value { return puff.Field(m, k) }
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
	project, _ := get("project").(puff.String)
	return &timer{key: key, due: due.Go(), addr: to, m: message{from: name("from"), fn: fn, args: puff.Print(args), dir: string(dir),
		lineage: lineage{user: name("origin") == string(origin(true)), reviewed: bool(reviewed), session: name("session"), project: string(project)}}}, nil
}

// stored is a as it is stored: as (inbox) shows it, with what a restart
// needs too, and :msg, its id in history, so its answer stays in its chain:
// message ids go on across restarts.
func (a *asked) stored() puff.Value {
	v := a.value().(puff.Map)
	with := func(key string, val puff.Value) {
		v, _ = v.With(puff.Keyword(key), val)
	}
	with("addr", puff.Keyword(a.addr))
	if a.msg != 0 {
		with("msg", puff.Int(a.msg))
	}
	if a.session != "" {
		with("session", puff.Keyword(a.session))
	}
	if a.task != 0 {
		with("parent", puff.Int(a.task))
	}
	if a.project != "" {
		with("project", puff.String(a.project))
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
		v := puff.Field(m, k)
		s, _ := v.(puff.Keyword)
		return string(s)
	}
	addr, fn := key("addr"), key("fn")
	if addr == "" || fn == "" {
		return nil, fmt.Errorf("want :addr and :fn, got %s", p)
	}
	get := func(k string) puff.Value { return puff.Field(m, k) }
	args, ok := get("args").(puff.Vector)
	if !ok {
		return nil, fmt.Errorf("want :args as a vector, got %s", p)
	}
	dir, _ := get("cwd").(puff.String)
	at, _ := get("at").(puff.Time)
	expires, _ := get("expires").(puff.Duration)
	msg, _ := get("msg").(puff.Int)
	parent, _ := get("parent").(puff.Int)
	project, _ := get("project").(puff.String)
	a := &asked{ticket: ticket{id: id, msg: int64(msg), from: key("from"), dir: string(dir), lineage: lineage{user: key("origin") == string(origin(true)), session: key("session"), task: int64(parent), project: string(project)}},
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
	a.about = a.label()
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
	held := message{from: a.from, args: puff.Print(a.args), dir: a.dir, lineage: a.unreviewed()}
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
