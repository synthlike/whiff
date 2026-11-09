package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// A far address is a peer's: :pipeline@vps/build is :pipeline/build on
// the peer vps. A message to one goes to the peer as this whiff's client
// would send it, in its origin, so the peer's review gate decides on an
// agent's; it runs there in the peer's dir, or where whiff call -C says.
// A task it begins there is a task here too, answered as the peer's ends.
// Only one hop: a peer's peers aren't reached through it.

// farAddr splits addr, a far address, into the address on the peer and
// the peer's name; ok is false for an address of this whiff's.
func farAddr(addr string) (far, peer string, ok bool) {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return "", "", false
	}
	return addr[:at], addr[at+1:], true
}

// farTries is how many times a message is sent to a peer whose connection
// goes before it answers: it has a submission key, so it runs there once.
const farTries = 3

// farSendWait bounds how long a send waits for its peer to come back;
// a call waits as long as its caller does.
var farSendWait = time.Minute

// forward sends m, to the far address addr, to its peer, and answers its
// sender, on m.reply, once the peer has. On the route goroutine; nil once
// it is on its way, as for a message in a mailbox.
func (d *daemon) forward(st *state, addr string, m message) puff.Value {
	far, name, _ := farAddr(addr)
	p := st.peers[name]
	var res puff.Value
	switch {
	case far == "" || name == "":
		res = puff.Errorf("no-address", "want a far address as :address@peer, got :%s", addr)
	case strings.Contains(far, "@"):
		res = puff.Errorf("no-address", ":%s is a peer's peer: whiff reaches only its own peers", addr)
	case m.topic != "" || m.notice():
		res = puff.Errorf("no-address", "%s can't go to a peer", m.to(addr))
	case p == nil:
		res = puff.Errorf("no-peer", "no peer %s, for :%s: whiff peer add %s user@host", name, m.to(addr), name)
	case p.client == nil:
		why := ""
		if p.err != "" {
			why = ": " + p.err
		}
		res = puff.Errorf("offline", "peer %s is %s%s", name, p.status, why)
	}
	if res != nil {
		st.record(m.entry(time.Now(), m.to(addr), puff.Print(res), ""))
		return res
	}
	dir := p.dir
	switch {
	case filepath.IsAbs(m.there) || dir == "":
		dir = m.there
	case m.there != "":
		dir = filepath.Join(dir, m.there)
	}
	req := rpc.Request{Op: m.kind(), Target: ":" + far + "/" + m.fn, Args: m.args, Dir: dir, Key: newKey(), Read: m.read,
		// One you let through here goes on as yours there.
		Agent: !m.user && !m.reviewed}
	go func() {
		ctx, cancel := m.ctx, context.CancelFunc(func() {})
		if ctx == nil {
			ctx, cancel = context.WithTimeout(d.ctx, farSendWait)
		}
		defer cancel()
		began := time.Now()
		r := d.submit(ctx, name, req)
		var follow int64
		d.do(func(st *state) {
			if n, ok := pendingID(r); ok {
				r, follow = d.farTask(st, addr, m, n), n
			} else {
				r = farActions(r, name)
			}
			e := m.entry(began, m.to(addr), puff.Print(r), "")
			e.took = time.Since(began)
			st.record(e)
		})
		if m.reply != nil {
			m.reply <- puff.Print(r)
		}
		// It may have ended before it was followed here.
		if follow != 0 {
			d.checkFar(name, nil, []int64{follow})
		}
	}()
	return nil
}

// newKey is a new submission key.
func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// submit sends req to the peer name and returns its Result: again, with
// the same key, on a new connection if the one it went on went before it
// answered, until ctx ends.
func (d *daemon) submit(ctx context.Context, name string, req rpc.Request) puff.Value {
	var last *rpc.Client
	var err error
	for range farTries {
		c := d.peerConn(ctx, name, last)
		if c == nil {
			break
		}
		var resp rpc.Response
		if resp, err = c.Forward(req); err == nil {
			if resp.Error != "" {
				return puff.Errorf("peer", "peer %s: %s", name, resp.Error)
			}
			v, err := puff.ReadValue(resp.Value)
			if err != nil {
				return puff.Errorf("peer", "peer %s answered %q: %v", name, resp.Value, err)
			}
			return v
		}
		last = c
	}
	why := "it didn't come back"
	if err != nil {
		why = err.Error()
	}
	return failed("offline", "unknown", "peer %s went before it answered %s: %s", name, req.Target, why)
}

// peerConn is the peer name's connection once it has one that isn't
// not; nil if ctx ends first.
func (d *daemon) peerConn(ctx context.Context, name string, not *rpc.Client) *rpc.Client {
	for {
		var c *rpc.Client
		d.do(func(st *state) {
			if p := st.peers[name]; p != nil && p.client != not {
				c = p.client
			}
		})
		if c != nil {
			return c
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// farTask follows task n of the peer, which m, to the far address addr,
// began there, as a task here: its sender gets (err {:kind :pending :id})
// of it, and whiff/done as n ends. It waits in addr's inbox, as an ask's
// task does, and is stored, so it is followed after a restart too.
func (d *daemon) farTask(st *state, addr string, m message, n int64) puff.Value {
	_, name, _ := farAddr(addr)
	v, _ := puff.ReadValue(m.args)
	args, _ := v.(puff.Vector)
	a := &asked{ticket: ticket{id: st.newID(), msg: m.id, from: m.from, dir: m.dir, lineage: m.lineage}, addr: addr, fn: m.fn, args: args, at: time.Now(), remote: n}
	a.about = a.label()
	if err := st.keep(a); err != nil {
		return puff.Errorf("store", "%s is task %d on %s, which isn't followed here: %v", m.to(addr), n, name, err)
	}
	st.asked = append(st.asked, a)
	st.taskChanged(a.id, m.user)
	return pending(a.id, ":%s is task %d on %s", m.to(addr), n, name)
}

// farCheckEvery is how often the peer's tasks followed here are checked,
// should an event that one ended have been dropped.
var farCheckEvery = 5 * time.Minute

// followFar answers the tasks followed here of the peer name, as they end
// there: each as c connects, then each its :tasks/changed says changed,
// until c goes or ctx ends.
func (d *daemon) followFar(ctx context.Context, name string, c *rpc.Client) {
	d.checkFar(name, c, nil)
	tick := time.NewTicker(farCheckEvery)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-c.Events():
			if !ok {
				return
			}
			// A published value comes as itself: the task's id.
			v, _ := puff.ReadValue(e.Value)
			if n, ok := v.(puff.Int); ok && e.Topic == "tasks/changed" {
				d.checkFar(name, c, []int64{int64(n)})
			}
		case <-tick.C:
			d.checkFar(name, c, nil)
		case <-ctx.Done():
			return
		}
	}
}

// checkFar answers each task followed here of the peer name whose task
// there, one of ids or any if ids is nil, has ended: asking it on c, or
// the peer's connection if c is nil.
func (d *daemon) checkFar(name string, c *rpc.Client, ids []int64) {
	type followed struct{ id, remote int64 }
	var fs []followed
	d.do(func(st *state) {
		if p := st.peers[name]; c == nil && p != nil {
			c = p.client
		}
		for _, a := range st.asked {
			if _, peer, ok := farAddr(a.addr); ok && a.remote != 0 && peer == name && (ids == nil || slices.Contains(ids, a.remote)) {
				fs = append(fs, followed{a.id, a.remote})
			}
		}
	})
	if c == nil {
		return
	}
	for _, f := range fs {
		resp, err := c.Forward(rpc.Request{Op: "call", Target: ":tasks/result", Args: fmt.Sprintf("[%d]", f.remote)})
		if err != nil {
			return
		}
		v, _ := puff.ReadValue(resp.Value)
		t, ok := v.(puff.Tag)
		if !ok || t.Name() != "ok" {
			continue
		}
		d.do(func(st *state) {
			if a := st.take("", f.id); a != nil {
				d.answer(st, a.ticket, a.addr, farActions(t.At(0), name))
			}
		})
	}
}

// cancelFar cancels a's task on its peer, a's task here having been
// cancelled; noted if the peer can't be told.
func (d *daemon) cancelFar(a *asked) {
	_, name, _ := farAddr(a.addr)
	ctx, cancel := context.WithTimeout(d.ctx, farSendWait)
	defer cancel()
	r := d.submit(ctx, name, rpc.Request{Op: "cancel-task", Args: fmt.Sprintf("[%d]", a.remote)})
	if t, ok := r.(puff.Tag); !ok || t.Name() != "ok" {
		d.note("task %d on %s, cancelled here as task %d, may still run there: %s", a.remote, name, a.id, puff.Print(r))
	}
}

// farPub publishes m, to a peer's topic, :topic@peer, there.
func (d *daemon) farPub(m message) puff.Value {
	topic, name, _ := farAddr(m.topic)
	if topic == "" || name == "" || strings.Contains(topic, "@") {
		return puff.Errorf("no-address", "want a far topic as :topic@peer, got :%s", m.topic)
	}
	ctx, cancel := context.WithTimeout(d.ctx, farSendWait)
	defer cancel()
	var r puff.Value = puff.Errorf("no-peer", "no peer %s, for :%s", name, m.topic)
	var there bool
	d.do(func(st *state) { there = st.peers[name] != nil })
	if there {
		r = d.submit(ctx, name, rpc.Request{Op: "pub", Target: ":" + topic, Args: m.args, Agent: !m.user && !m.reviewed})
	}
	d.do(func(st *state) {
		st.record(entry{at: time.Now(), kind: "pub", id: st.msgID(), cause: m.cause, user: m.user, from: m.from, to: name + " on :" + topic, args: m.args, reply: puff.Print(r), project: m.project})
	})
	return r
}

// farActions is v, a value from the peer name, with the targets of its
// :action vectors, as a view's items have, made far: [:review/accept 3]
// is [:review@vps/accept 3] here.
func farActions(v puff.Value, name string) puff.Value {
	switch v := v.(type) {
	case puff.Vector:
		items := make([]puff.Value, v.Len())
		for i := range v.Len() {
			items[i] = farActions(v.At(i), name)
		}
		return puff.NewVector(items...)
	case puff.Tag:
		args := make([]puff.Value, v.Len())
		for i := range v.Len() {
			args[i] = farActions(v.At(i), name)
		}
		return puff.NewTag(v.Name(), args...)
	case puff.Map:
		es := make([]puff.Entry, v.Len())
		for i := range v.Len() {
			e := v.Entry(i)
			if act, ok := e.Val.(puff.Vector); ok && puff.Equal(e.Key, puff.Keyword("action")) && act.Len() > 0 {
				if k, ok := act.At(0).(puff.Keyword); ok {
					if slash := strings.LastIndexByte(string(k), '/'); slash > 0 && !strings.Contains(string(k), "@") {
						items := []puff.Value{puff.Keyword(string(k[:slash]) + "@" + name + string(k[slash:]))}
						for j := 1; j < act.Len(); j++ {
							items = append(items, act.At(j))
						}
						es[i] = puff.Entry{Key: e.Key, Val: puff.NewVector(items...)}
						continue
					}
				}
			}
			es[i] = puff.Entry{Key: e.Key, Val: farActions(e.Val, name)}
		}
		m, err := puff.NewMap(es...)
		if err != nil {
			return v
		}
		return m
	}
	return v
}

// keysBucket holds the Response to each call or send that came with a
// submission key, by key, printed: {:at :value :error}, for keyKept.
var keysBucket = []byte("keys")

// keyKept is how long a submission key's Response is kept: a peer sends
// again within minutes, as its connection comes back.
const keyKept = 24 * time.Hour

// once runs a call or send that came with the submission key, or gives
// the Response of the one that came before with it, waiting for it if it
// is still running.
func (d *daemon) once(key string, run func() rpc.Response) rpc.Response {
	if len(key) > 64 {
		return rpc.Response{Error: "a submission key is at most 64 characters"}
	}
	for {
		var kept *rpc.Response
		var wait chan struct{}
		d.do(func(st *state) {
			if r, ok := keyedResponse(st.db, key); ok {
				kept = &r
			} else if wait = st.keyed[key]; wait == nil {
				st.keyed[key] = make(chan struct{})
			}
		})
		switch {
		case kept != nil:
			return *kept
		case wait == nil:
			resp := run()
			d.do(func(st *state) {
				if err := keepKeyed(st.db, key, resp); err != nil {
					st.note(fmt.Sprintf("a submission key isn't stored, so a message sent again with it would run again: %v", err))
				}
				close(st.keyed[key])
				delete(st.keyed, key)
			})
			return resp
		}
		select {
		case <-wait:
		case <-d.ctx.Done():
			return rpc.Response{Error: "whiff is stopping"}
		}
	}
}

// keyedResponse is the Response kept for key, if there is one.
func keyedResponse(db *bbolt.DB, key string) (rpc.Response, bool) {
	var resp rpc.Response
	var ok bool
	db.View(func(tx *bbolt.Tx) error {
		p := tx.Bucket(keysBucket).Get([]byte(key))
		if p == nil {
			return nil
		}
		get, err := fields(string(p))
		if err != nil {
			return nil
		}
		v, _ := get("value").(puff.String)
		e, _ := get("error").(puff.String)
		resp, ok = rpc.Response{Value: string(v), Error: string(e)}, true
		return nil
	})
	return resp, ok
}

// keepKeyed keeps resp as key's.
func keepKeyed(db *bbolt.DB, key string, resp rpc.Response) error {
	m, err := puff.NewMap(puff.Key("at", puff.NewTime(time.Now())), puff.Key("value", puff.String(resp.Value)), puff.Key("error", puff.String(resp.Error)))
	if err != nil {
		return err
	}
	return db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(keysBucket).Put([]byte(key), []byte(puff.Print(m)))
	})
}

// pruneKeys deletes the submission keys kept since before cutoff.
func pruneKeys(tx *bbolt.Tx, cutoff time.Time) error {
	b := tx.Bucket(keysBucket)
	var old [][]byte
	b.ForEach(func(k, v []byte) error {
		var at puff.Value
		if get, err := fields(string(v)); err == nil {
			at = get("at")
		}
		if t, ok := at.(puff.Time); !ok || t.Go().Before(cutoff) {
			old = append(old, slices.Clone(k))
		}
		return nil
	})
	for _, k := range old {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
