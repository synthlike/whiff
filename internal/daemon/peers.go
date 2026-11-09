package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// A peer is another machine's whiff this one reaches as a client, by ssh
// host whiff bridge: :peers lists them and says how each is connected.
// This whiff drives; a peer only answers what it is asked, so it can't act
// as you here. Each is kept in whiff.db, and a goroutine of its own keeps
// it connected, again and again with backoff, until it is removed or whiff
// stops.

// peersBucket holds each peer, printed, by name: {:dest :whiff :dir} or
// {:cmd [...]}.
var peersBucket = []byte("peers")

// peer is a peer as the route goroutine keeps it: how to reach it and how
// it is connected now.
type peer struct {
	name  string
	dest  string   // user@host, for ssh
	whiff string   // whiff's path on the host, "whiff" by default
	dir   string   // where its calls run on the host, if not its whiff's own
	cmd   []string // the command that reaches it, if given instead of dest

	status string    // "connecting", "connected" or "offline"
	since  time.Time // when status last changed
	seen   time.Time // when it was last connected; zero if never
	err    string    // why it isn't connected, if it isn't
	client *rpc.Client
	stop   context.CancelFunc
}

// argv is the command that reaches p: its cmd, or ssh to its dest running
// whiff bridge there, without asking for a password, as no one is there to
// type one.
func (p *peer) argv() []string {
	if len(p.cmd) > 0 {
		return p.cmd
	}
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3", p.dest, p.whiff, "bridge"}
}

// stored is p as whiff.db holds it.
func (p *peer) stored() puff.Value {
	var es []puff.Entry
	if len(p.cmd) > 0 {
		cmd := make([]puff.Value, len(p.cmd))
		for i, a := range p.cmd {
			cmd[i] = puff.String(a)
		}
		es = append(es, puff.Key("cmd", puff.NewVector(cmd...)))
	} else {
		es = append(es, puff.Key("dest", puff.String(p.dest)), puff.Key("whiff", puff.String(p.whiff)))
	}
	if p.dir != "" {
		es = append(es, puff.Key("dir", puff.String(p.dir)))
	}
	m, _ := puff.NewMap(es...)
	return m
}

// newPeer is the peer name that opts, {:dest :whiff :dir} or {:cmd [...]},
// describe, or why they don't.
func newPeer(name string, opts puff.Value) (*peer, error) {
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		return nil, fmt.Errorf("a peer's name is lowercase letters, digits, - and _, as vps, not %q", name)
	}
	m, ok := opts.(puff.Map)
	if !ok {
		return nil, fmt.Errorf("want a peer as {:dest \"user@host\"}, got %s", puff.Print(opts))
	}
	p := &peer{name: name, whiff: "whiff"}
	str := func(k string) (string, error) {
		v, ok := m.Get(puff.Keyword(k))
		if !ok {
			return "", nil
		}
		s, ok := v.(puff.String)
		if !ok || s == "" {
			return "", fmt.Errorf(":%s wants a string, got %s", k, puff.Print(v))
		}
		return string(s), nil
	}
	var err error
	if p.dest, err = str("dest"); err != nil {
		return nil, err
	}
	if p.dir, err = str("dir"); err != nil {
		return nil, err
	}
	if w, err := str("whiff"); err != nil {
		return nil, err
	} else if w != "" {
		p.whiff = w
	}
	if v, ok := m.Get(puff.Keyword("cmd")); ok {
		vec, ok := v.(puff.Vector)
		if !ok || vec.Len() == 0 {
			return nil, fmt.Errorf(":cmd wants a command, as [\"ssh\" \"vps\" \"whiff\" \"bridge\"], got %s", puff.Print(v))
		}
		for i := range vec.Len() {
			s, ok := vec.At(i).(puff.String)
			if !ok {
				return nil, fmt.Errorf(":cmd wants strings, got %s", puff.Print(vec.At(i)))
			}
			p.cmd = append(p.cmd, string(s))
		}
	}
	if (p.dest == "") == (len(p.cmd) == 0) {
		return nil, errors.New("a peer wants :dest, as \"user@host\", or :cmd, the command that reaches it, not both")
	}
	return p, nil
}

// storedPeers are the peers in whiff.db. One it can't read is left out.
func (d *daemon) storedPeers() []*peer {
	var ps []*peer
	d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(peersBucket).ForEach(func(k, v []byte) error {
			opts, err := puff.ReadValue(string(v))
			if err == nil {
				if p, err := newPeer(string(k), opts); err == nil {
					ps = append(ps, p)
				}
			}
			return nil
		})
	})
	return ps
}

// startPeers connects every stored peer, as whiff starts.
func (d *daemon) startPeers() {
	for _, p := range d.storedPeers() {
		d.do(func(st *state) { d.startPeer(st, p) })
	}
}

// startPeer keeps p connected from now on, in place of any peer of its
// name, which it disconnects. On the route goroutine.
func (d *daemon) startPeer(st *state, p *peer) {
	if old := st.peers[p.name]; old != nil {
		old.stop()
	}
	ctx, stop := context.WithCancel(d.ctx)
	p.stop, p.status, p.since = stop, "connecting", time.Now()
	st.peers[p.name] = p
	d.wg.Go(func() { d.keepPeer(ctx, p) })
}

// peerWait is how long a peer waits before it tries again, doubled each
// time up to peerWaitMost; back to peerWait once a connection has lasted
// peerWaitMost.
var peerWait, peerWaitMost = time.Second, time.Minute

// keepPeer connects p, and again whenever its connection ends, until ctx
// ends: as p is removed or replaced, or whiff stops.
func (d *daemon) keepPeer(ctx context.Context, p *peer) {
	wait := peerWait
	for {
		d.setPeer(p, "connecting", "", nil)
		c, err := rpc.DialCommand(ctx, p.argv(), nil)
		if err == nil {
			if err = c.PeerCompatible(p.name); err == nil {
				// Before anything is sent on it, so no task it begins
				// there ends unseen.
				_, err = c.Sub("tasks/changed")
			}
			if err != nil {
				c.Close()
			}
		}
		if err == nil {
			began := time.Now()
			d.setPeer(p, "connected", "", c)
			d.wg.Go(func() { d.followFar(ctx, p.name, c) })
			select {
			case <-c.Done():
				err = errors.New("the connection ended")
			case <-ctx.Done():
			}
			c.Close()
			if time.Since(began) >= peerWaitMost {
				wait = peerWait
			}
		}
		if ctx.Err() != nil {
			return
		}
		d.setPeer(p, "offline", err.Error(), nil)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		wait = min(wait*2, peerWaitMost)
	}
}

// setPeer says how p is connected now, if it is still the peer of its
// name, and publishes :peers/changed if that changed.
func (d *daemon) setPeer(p *peer, status, why string, c *rpc.Client) {
	changed := false
	d.do(func(st *state) {
		if st.peers[p.name] != p {
			return
		}
		now := time.Now()
		if p.status == "connected" || status == "connected" {
			p.seen = now
		}
		changed = p.status != status || p.err != why
		if p.status != status {
			p.since = now
		}
		p.status, p.err, p.client = status, why, c
	})
	if changed {
		go d.publish(message{from: "peers", topic: "peers/changed", args: "[:changed]", lineage: lineage{user: true}})
	}
}

// peerValues are the peers, by name, as :peers/list gives them.
func (d *daemon) peerValues() []puff.Value {
	var ps []peer
	d.do(func(st *state) {
		for _, p := range st.peers {
			ps = append(ps, *p)
		}
	})
	slices.SortFunc(ps, func(a, b peer) int { return strings.Compare(a.name, b.name) })
	vs := make([]puff.Value, len(ps))
	for i, p := range ps {
		es := []puff.Entry{puff.Key("name", puff.String(p.name))}
		add := func(k, v string) {
			if v != "" {
				es = append(es, puff.Key(k, puff.String(v)))
			}
		}
		add("dest", p.dest)
		add("dir", p.dir)
		if len(p.cmd) > 0 {
			add("cmd", strings.Join(p.cmd, " "))
		}
		es = append(es, puff.Key("status", puff.Keyword(p.status)), puff.Key("since", puff.NewTime(p.since)))
		if !p.seen.IsZero() {
			es = append(es, puff.Key("seen", puff.NewTime(p.seen)))
		}
		add("error", p.err)
		vs[i], _ = puff.NewMap(es...)
	}
	return vs
}

// peerItem is a peer, as :peers/list has it, as an item of :peers/view:
// its name and status, then where it is, when it was last seen and why it
// isn't connected; Remove.
func peerItem(v puff.Value, now time.Time) puff.Value {
	m := v.(puff.Map)
	str := func(k string) string { s, _ := puff.Field(m, k).(puff.String); return string(s) }
	status, _ := puff.Field(m, "status").(puff.Keyword)
	parts := []string{str("dest") + str("cmd")}
	if d := str("dir"); d != "" {
		parts = append(parts, d)
	}
	if status != "connected" {
		if seen, ok := puff.Field(m, "seen").(puff.Time); ok {
			parts = append(parts, "seen "+roughly(now.Sub(seen.Go()))+" ago")
		} else {
			parts = append(parts, "never connected")
		}
	}
	if e := str("error"); e != "" {
		parts = append(parts, e)
	}
	rm, _ := puff.NewMap(puff.Key("label", puff.String("Remove")), puff.Key("action", puff.NewVector(puff.Keyword("peers/rm"), puff.String(str("name")))))
	item, _ := puff.NewMap(
		puff.Key("label", puff.String(str("name")+"  "+string(status))),
		puff.Key("detail", puff.String(strings.Join(parts, " · "))),
		puff.Key("actions", puff.NewVector(rm)),
	)
	return item
}

// addPeer stores the peer name, as opts describe it, and connects it, in
// place of one of its name.
func (d *daemon) addPeer(name string, opts puff.Value) puff.Value {
	p, err := newPeer(name, opts)
	if err != nil {
		return puff.Errorf("bad-peer", "%v", err)
	}
	err = d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(peersBucket).Put([]byte(name), []byte(puff.Print(p.stored())))
	})
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	d.do(func(st *state) { d.startPeer(st, p) })
	return puff.Done
}

// removePeer disconnects the peer name and forgets it.
func (d *daemon) removePeer(name string) puff.Value {
	found := false
	err := d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(peersBucket)
		found = b.Get([]byte(name)) != nil
		return b.Delete([]byte(name))
	})
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	if !found {
		return puff.Errorf("no-peer", "no peer %s", name)
	}
	d.do(func(st *state) {
		if p := st.peers[name]; p != nil {
			p.stop()
			delete(st.peers, name)
		}
	})
	go d.publish(message{from: "peers", topic: "peers/changed", args: "[:changed]", lineage: lineage{user: true}})
	return puff.Done
}

// peersService is :peers.
func (d *daemon) peersService() *service {
	name := func(v puff.Value) (string, error) {
		s, ok := v.(puff.String)
		if !ok {
			return "", fmt.Errorf("want a peer's name, a string like \"vps\", got %s", puff.Print(v))
		}
		return string(s), nil
	}
	return &service{name: "peers", d: d, cur: caller{addr: "peers"},
		fns: map[string]handler{
			"list": {mark: puff.MarkRead, marked: true,
				doc: "(ok peers): the other machines' whiffs this one reaches, by name, each {:name :dest :dir :status :since :seen :error}: :status :connecting, :connected or :offline, since when, when it was last connected, and why it isn't. :cmd instead of :dest for one reached by a command of its own.",
				native: func(message, []puff.Value) (puff.Value, error) {
					return puff.Ok(puff.NewVector(d.peerValues()...)), nil
				}},
			"view": {mark: puff.MarkRead, marked: true,
				doc: "(ok view): the peers as list gives them, as items, each with Remove; following :peers/changed, which is published as one is added, removed, connects or goes.",
				native: func(message, []puff.Value) (puff.Value, error) {
					now := time.Now()
					var items []puff.Value
					for _, v := range d.peerValues() {
						items = append(items, peerItem(v, now))
					}
					opts, _ := puff.NewMap(puff.Key("changed", puff.Keyword("peers/changed")))
					return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
				}},
			"add": {params: []string{"name", "peer"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Adds peer name, another machine's whiff, and connects to it, by ssh dest whiff bridge, again whenever the connection goes: peer is {:dest \"user@host\"}, with :whiff, whiff's path there, if ssh doesn't find it, and :dir, where calls run there; or {:cmd [...]}, the command that reaches it. One of the same name is replaced. :done, or (err {:kind :bad-peer}).",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					n, err := name(args[0])
					if err != nil {
						return nil, err
					}
					return d.addPeer(n, args[1]), nil
				}},
			"rm": {params: []string{"name"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Disconnects peer name and forgets it: :done, or (err {:kind :no-peer}).",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					n, err := name(args[0])
					if err != nil {
						return nil, err
					}
					return d.removePeer(n), nil
				}},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}
