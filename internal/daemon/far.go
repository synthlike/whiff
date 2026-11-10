package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// farName is name, a peer's address/fn or topic, as this whiff names it:
// the peer after its address, :pipeline/changed of vps is
// :pipeline@vps/changed, :agent/1/inbox :agent/1@vps/inbox, and one
// without a slash ends in it, :box@vps.
func farName(name, peer string) string {
	if i := strings.LastIndexByte(name, '/'); i > 0 {
		return name[:i] + "@" + peer + name[i:]
	}
	return name + "@" + peer
}

// splitFar is a far topic's name on its peer, and the peer's: the
// reverse of farName. ok is false for a name of this whiff's, or of a
// peer's peer, or one without a peer's name.
func splitFar(name string) (local, peer string, ok bool) {
	at := strings.IndexByte(name, '@')
	if at <= 0 || strings.Count(name, "@") > 1 {
		return "", "", false
	}
	end := len(name)
	if i := strings.IndexByte(name[at:], '/'); i >= 0 {
		end = at + i
	}
	if end == at+1 {
		return "", "", false
	}
	return name[:at] + name[end:], name[at+1 : end], true
}

// farTries is how many times a message is sent to a peer whose connection
// goes before it answers: it has a submission key, so it runs there once.
const farTries = 3

// coming says whether p, not connected, is likely to be soon, so a
// message waits for it: it is connecting, as whiff has just started, or
// was connected a moment ago, and goes on trying.
func (p *peer) coming() bool {
	return p.status == "connecting" || !p.seen.IsZero() && time.Since(p.seen) < farRecent
}

// farRecent is how lately a peer that isn't connected was, for a message
// to wait for it rather than fail at once.
var farRecent = time.Minute

// farConnectWait is how long a message waits for a peer that is
// connecting, before it goes.
var farConnectWait = 15 * time.Second

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
	case p.client == nil && !p.coming():
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

// atPeer sends req, an inbox, reply, accept, reject or cancel-task for
// its peer, there, as this whiff's client, in req's origin, so the peer
// decides as it would on one of its own clients': an agent can't accept
// there either. Once: they have no key, and one sent again says the
// message has gone.
func (d *daemon) atPeer(req rpc.Request) rpc.Response {
	switch req.Op {
	case "inbox", "reply", "accept", "reject", "cancel-task":
	default:
		return rpc.Response{Error: fmt.Sprintf("%s isn't for a peer: of these, only inbox, reply, accept, reject and cancel", req.Op)}
	}
	name := req.Peer
	req.Peer = ""
	var c *rpc.Client
	var why string
	connecting := false
	d.do(func(st *state) {
		switch p := st.peers[name]; {
		case p == nil:
			why = fmt.Sprintf("no peer %s", name)
		case p.client == nil:
			why = fmt.Sprintf("peer %s is %s", name, p.status)
			if p.err != "" {
				why += ": " + p.err
			}
			connecting = p.coming()
		default:
			c = p.client
		}
	})
	// One still connecting, as whiff has just started, is waited for.
	if connecting {
		ctx, cancel := context.WithTimeout(d.ctx, farConnectWait)
		c = d.peerConn(ctx, name, nil)
		cancel()
		why = fmt.Sprintf("peer %s didn't connect within %v", name, farConnectWait)
	}
	if c == nil {
		return rpc.Response{Error: why}
	}
	resp, err := c.Forward(req)
	if err != nil {
		return rpc.Response{Error: fmt.Sprintf("peer %s went before it answered: %v", name, err)}
	}
	return resp
}

// farViewRead is the Read a view here sends for a peer's part of it: a
// view that gets it leaves its own peers out, so two whiffs that are each
// other's peers don't ask each other round and round.
const farViewRead = "a peer's view"

// farViewWait is how long a view here waits for a peer's part of it.
var farViewWait = 3 * time.Second

// farViews are the items of the view target, address/fn, on each
// connected peer, made far, each label after the peer's name, "vps: ...",
// so this whiff's queues are every peer's too; and the topics those views
// follow, made far, for a page to follow. A peer that doesn't answer
// within farViewWait is left out.
func (d *daemon) farViews(m message, target string) (items, topics []puff.Value) {
	if m.read == farViewRead {
		return nil, nil
	}
	type part struct {
		name string
		c    *rpc.Client
		view puff.Value
	}
	var parts []*part
	d.do(func(st *state) {
		for name, p := range st.peers {
			if p.client != nil {
				parts = append(parts, &part{name: name, c: p.client})
			}
		}
	})
	slices.SortFunc(parts, func(a, b *part) int { return strings.Compare(a.name, b.name) })
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, pt := range parts {
		wg.Go(func() {
			got := make(chan puff.Value, 1)
			go func() {
				resp, err := pt.c.Forward(rpc.Request{Op: "call", Target: ":" + target, Args: "[]", Read: farViewRead})
				v, _ := puff.ReadValue(resp.Value)
				if err != nil || v == nil {
					v = puff.Done
				}
				got <- v
			}()
			select {
			case v := <-got:
				mu.Lock()
				pt.view = farActions(puff.OkValue(v), pt.name)
				mu.Unlock()
			case <-time.After(farViewWait):
			}
		})
	}
	wg.Wait()
	for _, pt := range parts {
		mu.Lock()
		t, ok := pt.view.(puff.Tag)
		mu.Unlock()
		if !ok || t.Name() != "items" || t.Len() == 0 {
			continue
		}
		vec, _ := t.At(0).(puff.Vector)
		for i := range vec.Len() {
			item, ok := vec.At(i).(puff.Map)
			if !ok {
				continue
			}
			if l, ok := puff.Field(item, "label").(puff.String); ok {
				item, _ = item.With(puff.Keyword("label"), puff.String(pt.name+": "+string(l)))
			}
			items = append(items, item)
		}
		if t.Len() > 1 {
			opts, _ := t.At(1).(puff.Map)
			switch ch := puff.Field(opts, "changed").(type) {
			case puff.Keyword:
				topics = append(topics, ch)
			case puff.Vector:
				for j := range ch.Len() {
					topics = append(topics, ch.At(j))
				}
			}
		}
	}
	return items, topics
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
	for try := range farTries {
		var c *rpc.Client
		if try == 0 {
			// A peer still connecting, as whiff has just started, is
			// waited for, a while.
			first, cancel := context.WithTimeout(ctx, farConnectWait)
			c = d.peerConn(first, name, nil)
			cancel()
			if c == nil {
				return failed("offline", "not-started", "peer %s didn't connect within %v", name, farConnectWait)
			}
		} else if c = d.peerConn(ctx, name, last); c == nil {
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

// followFar keeps up with the peer p on c, from as it connects until c
// goes or ctx ends: hist, its history since it was last seen, and each
// new entry, are mirrored into history here; what it publishes is
// published here, its topics made far; and the tasks followed here are
// answered as they end there, each checked as it connects and every
// farCheckEvery. Its cursor is stored as it goes, now and then.
func (d *daemon) followFar(ctx context.Context, p *peer, c *rpc.Client, hist []rpc.Entry) {
	d.do(func(st *state) {
		for _, e := range hist {
			st.mirror(p, e)
		}
	})
	saved := d.saveCursor(p, rpc.Entry{})
	defer d.saveCursor(p, saved)
	d.checkFar(p.name, c, nil)
	check, save := time.NewTicker(farCheckEvery), time.NewTicker(cursorEvery)
	defer check.Stop()
	defer save.Stop()
	for {
		select {
		case e, ok := <-c.Entries():
			if !ok {
				return
			}
			d.do(func(st *state) { st.mirror(p, e) })
		case e, ok := <-c.Events():
			if !ok {
				return
			}
			if strings.Contains(e.Topic, "@") {
				continue
			}
			// One value, or it is dropped: else "1] [2" would publish two.
			v, err := puff.ReadValue(e.Value)
			if err != nil {
				continue
			}
			from := e.From
			if from != "" {
				from += "@" + p.name
			}
			d.publish(message{from: from, topic: farName(e.Topic, p.name), args: puff.Print(puff.NewVector(v)), lineage: lineage{user: e.Origin == "user"}})
			// A published value comes as itself: the task's id.
			if n, ok := v.(puff.Int); ok && e.Topic == "tasks/changed" {
				d.checkFar(p.name, c, []int64{int64(n)})
			}
		case <-check.C:
			d.checkFar(p.name, c, nil)
		case <-save.C:
			saved = d.saveCursor(p, saved)
		case <-ctx.Done():
			return
		}
	}
}

// cursorEvery is how often a peer's cursor is stored while its history
// comes: should whiff die, what came since comes again.
var cursorEvery = 5 * time.Second

// saveCursor stores p's cursor if it isn't saved, the one stored last,
// and returns the one stored now.
func (d *daemon) saveCursor(p *peer, saved rpc.Entry) rpc.Entry {
	var cur rpc.Entry
	var err error
	d.do(func(st *state) {
		if cur = p.cursor; !cur.At.Equal(saved.At) || cur.ID != saved.ID {
			err = storeCursor(st.db, p)
		}
	})
	if err != nil {
		d.note("peer %s's place in its history isn't stored, so some of it may come again: %v", p.name, err)
		return saved
	}
	return cur
}

// storeCursor stores p's cursor, unless it has none or p was removed. On
// the route goroutine, which owns it.
func storeCursor(db *bbolt.DB, p *peer) error {
	if p.cursor.At.IsZero() {
		return nil
	}
	m, _ := puff.NewMap(puff.Key("at", puff.NewTime(p.cursor.At)), puff.Key("id", puff.Int(p.cursor.ID)))
	return db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(peersBucket).Get([]byte(p.name)) == nil {
			return nil
		}
		return tx.Bucket(metaBucket).Put(cursorKey(p.name), []byte(puff.Print(m)))
	})
}

// farIDs bounds the ids a peer's messages keep here: past it they start
// again, and what an older one led to begins a chain of its own.
const farIDs = 10000

// mirror records e, an entry of the peer p's history, in history here,
// its addresses made far and its message given an id here, and moves p's
// cursor past it. What the peer has from its own peers isn't: only one
// hop.
func (st *state) mirror(p *peer, re rpc.Entry) {
	p.cursor = re
	if re.Peer != "" || strings.Contains(re.From+re.To, "@") {
		return
	}
	e := entry{at: re.At, kind: re.Kind, user: re.Origin == "user", args: re.Args, reply: re.Reply, took: re.Took, gate: re.Gate, project: re.Project, peer: p.name}
	if re.Kind == "note" {
		e.note = p.name + ": " + re.Note
		st.record(e)
		return
	}
	if p.ids == nil || len(p.ids) >= farIDs {
		p.ids = map[int64]int64{}
	}
	e.id = st.msgID()
	p.ids[re.ID] = e.id
	e.cause = p.ids[re.Cause]
	if re.From != "" {
		e.from = re.From + "@" + p.name
	}
	switch addr, topic, on := strings.Cut(re.To, " on :"); {
	case on:
		e.to = addr + "@" + p.name + " on :" + farName(topic, p.name)
		if addr == "" {
			e.to = "on :" + farName(topic, p.name)
		}
	case re.To != "":
		e.to = farName(re.To, p.name)
	}
	st.record(e)
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
			// One being checked already, as its event comes as it is
			// followed, isn't asked about twice at once: it is asked
			// again once that check is done, as it may have ended since.
			if _, peer, ok := farAddr(a.addr); !ok || a.remote == 0 || peer != name || ids != nil && !slices.Contains(ids, a.remote) {
				continue
			}
			if st.checking[a.id] {
				st.checkAgain[a.id] = true
				continue
			}
			fs = append(fs, followed{a.id, a.remote})
			st.checking[a.id] = true
		}
	})
	defer func() {
		var again []int64
		d.do(func(st *state) {
			for _, f := range fs {
				delete(st.checking, f.id)
				if st.checkAgain[f.id] {
					delete(st.checkAgain, f.id)
					again = append(again, f.remote)
				}
			}
		})
		if len(again) > 0 {
			d.checkFar(name, nil, again)
		}
	}()
	if c == nil {
		return
	}
	for _, f := range fs {
		resp, err := c.Forward(rpc.Request{Op: "result", Args: fmt.Sprintf("[%d]", f.remote)})
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
				// Answered by an agent there, it is an agent's here, as
				// reply lowers one answered here.
				tk := a.ticket
				if resp.Origin != "user" {
					tk.user, tk.reviewed = false, false
				}
				d.answer(st, tk, a.addr, farActions(t.At(0), name))
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

// farPub publishes m, to a peer's topic, :git@vps/changed, there.
func (d *daemon) farPub(m message) puff.Value {
	topic, name, ok := splitFar(m.topic)
	if !ok {
		return puff.Errorf("no-address", "want a peer's topic as :git@vps/changed, got :%s", m.topic)
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
		st.record(entry{at: time.Now(), kind: "pub", id: st.msgID(), cause: m.cause, user: m.user, from: m.from, to: "on :" + m.topic, args: m.args, reply: puff.Print(r), project: m.project})
	})
	return r
}

// farActions is v, a value from the peer name, with what names that
// peer's addresses made far: the targets of its :action vectors, as a
// view's items have, and the topic a view's :changed follows. So
// [:review/accept 3] is [:review@vps/accept 3] here.
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
			es[i] = puff.Entry{Key: e.Key, Val: farActions(e.Val, name)}
			far := func(k puff.Value) (puff.Value, bool) {
				kw, ok := k.(puff.Keyword)
				if !ok || strings.Contains(string(kw), "@") {
					return k, false
				}
				return puff.Keyword(farName(string(kw), name)), true
			}
			switch {
			case puff.Equal(e.Key, puff.Keyword("changed")):
				if k, ok := far(e.Val); ok {
					es[i].Val = k
				} else if vec, ok := e.Val.(puff.Vector); ok {
					ks := make([]puff.Value, vec.Len())
					for j := range vec.Len() {
						ks[j], _ = far(vec.At(j))
					}
					es[i].Val = puff.NewVector(ks...)
				}
			case puff.Equal(e.Key, puff.Keyword("action")):
				act, ok := e.Val.(puff.Vector)
				if !ok || act.Len() == 0 {
					break
				}
				if k, ok := far(act.At(0)); ok && strings.Contains(string(act.At(0).(puff.Keyword)), "/") {
					items := []puff.Value{k}
					for j := 1; j < act.Len(); j++ {
						items = append(items, act.At(j))
					}
					es[i].Val = puff.NewVector(items...)
				}
			}
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
			// Stored off the route goroutine, before the waiters are let
			// go: they find it stored.
			err := keepKeyed(d.db, key, resp)
			d.do(func(st *state) {
				if err != nil {
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
