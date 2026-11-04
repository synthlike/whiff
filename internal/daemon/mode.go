package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// Modes: how the review gate treats an agent session, an :agent/<n>
// address. Gated, the default, holds its :write messages. Trusted lets its
// messages to the services you name through as if you had accepted them,
// so what those services do for it goes through too. Free lets everything
// it sends, publishes and evaluates through. Only you set a mode, as only
// you accept; it lasts until the session ends, or for a while. An agent's
// own session lasts as long as the agent, across restarts too.

// modesBucket holds the modes of agents' own sessions, :agent/<name>, by
// address, printed as (session/mode) gives them: trusting an agent
// outlasts a restart, as the agent does. A client's session, :agent/<n>,
// goes with its connection, and its mode with it, so it isn't stored.
var modesBucket = []byte("modes")

// mode is an agent session's mode other than gated.
type mode struct {
	free  bool
	trust map[string]bool // the services trusted mode lets through
	until time.Time       // zero for as long as the session lasts
}

// String is m as whiff ls shows it: free, or trusted :gh :todo, and the
// time left, if it has an end.
func (m *mode) String() string {
	s := "free"
	if !m.free {
		s = "trusted"
		for _, name := range slices.Sorted(maps.Keys(m.trust)) {
			s += " :" + name
		}
	}
	if !m.until.IsZero() {
		s += fmt.Sprintf(", %v left", time.Until(m.until).Round(time.Second))
	}
	return s
}

// name is m's name in history: trusted or free.
func (m *mode) name() string {
	if m.free {
		return "free"
	}
	return "trusted"
}

// modeOf is addr's mode, or nil if it is gated; a mode that has run out
// is gone, and history notes it.
func (st *state) modeOf(addr string) *mode {
	m := st.modes[addr]
	if m != nil && !m.until.IsZero() && !time.Now().Before(m.until) {
		st.setMode(addr, nil)
		st.record(entry{at: m.until, note: fmt.Sprintf(":%s's %s mode ran out: it is gated", addr, m.name())})
		return nil
	}
	return m
}

// heldGate is the gate of a message the review gate held, for history:
// held <id>, or refused if it couldn't wait (nobody reviews, or the inbox
// is full).
func heldGate(r puff.Value) string {
	if id, ok := pendingID(r); ok {
		return fmt.Sprintf("held %d", id)
	}
	return "refused"
}

// lets says whether from's mode lets its message to addr through, as if
// you had accepted it; addr "" is a publish, which only free lets through.
func (st *state) lets(from, addr string) bool {
	m := st.modeOf(from)
	return m != nil && (m.free || addr != "" && m.trust[addr])
}

// modeValue is addr's mode as (session/mode) gives it: (ok {:mode ...}), or (err
// {:kind :no-session}) if addr isn't an agent session.
func (st *state) modeValue(addr string) puff.Value {
	if s := st.addrs[addr]; s == nil || !s.client || !strings.HasPrefix(addr, "agent/") {
		return puff.Errorf("no-session", "only an agent session has a mode")
	}
	if m := st.modeOf(addr); m != nil {
		return puff.Ok(m.value())
	}
	v, _ := puff.NewMap(puff.Key("mode", puff.Keyword("gated")))
	return puff.Ok(v)
}

// value is m as (session/mode) gives it, and as it is stored: {:mode
// :trusted :services [:todo ...] :until t}, or {:mode :free}.
func (m *mode) value() puff.Value {
	es := []puff.Entry{puff.Key("mode", puff.Keyword(m.name()))}
	if !m.free {
		var services []puff.Value
		for _, name := range slices.Sorted(maps.Keys(m.trust)) {
			services = append(services, puff.Keyword(name))
		}
		es = append(es, puff.Key("services", puff.NewVector(services...)))
	}
	if !m.until.IsZero() {
		es = append(es, puff.Key("until", puff.NewTime(m.until)))
	}
	v, _ := puff.NewMap(es...)
	return v
}

// storedMode is the mode value printed, as p.
func storedMode(p string) (*mode, error) {
	get, err := fields(p)
	if err != nil {
		return nil, err
	}
	m := &mode{}
	switch get("mode") {
	case puff.Keyword("free"):
		m.free = true
	case puff.Keyword("trusted"):
		m.trust = map[string]bool{}
		services, _ := get("services").(puff.Vector)
		for i := range services.Len() {
			k, ok := services.At(i).(puff.Keyword)
			if !ok {
				return nil, fmt.Errorf("want services like :todo, got %s", puff.Print(services.At(i)))
			}
			m.trust[string(k)] = true
		}
	default:
		return nil, fmt.Errorf("want :free or :trusted, got %s", p)
	}
	if t, ok := get("until").(puff.Time); ok {
		m.until = t.Go()
	}
	return m, nil
}

// setMode makes m addr's mode, gated for nil, and stores it if addr is an
// agent's own session. One that can't be stored still holds until a
// restart, so it is only noted.
func (st *state) setMode(addr string, m *mode) {
	if m == nil {
		delete(st.modes, addr)
	} else {
		st.modes[addr] = m
	}
	if !agentSession(addr) {
		return
	}
	err := st.db.Update(func(tx *bbolt.Tx) error {
		if m == nil {
			return tx.Bucket(modesBucket).Delete([]byte(addr))
		}
		return tx.Bucket(modesBucket).Put([]byte(addr), []byte(puff.Print(m.value())))
	})
	if err != nil {
		st.note(fmt.Sprintf(":%s's mode isn't stored, so a restart forgets it: %v", addr, err))
	}
}

// clientMode sets an agent session's mode for a client: req.Target is the
// session's address, and req.Args a printed map {:mode :gated|:trusted|:free
// :services [:todo ...] :for 2h}, with :services for trusted and :for
// optional. Only your connection may: an agent can't set its own mode.
func (d *daemon) clientMode(req rpc.Request) rpc.Response {
	if req.Agent {
		return rpc.Response{Error: "an agent can't set a session's mode"}
	}
	addr := strings.TrimPrefix(req.Target, ":")
	v, err := puff.ReadValue(req.Args)
	args, ok := v.(puff.Map)
	if err != nil || !ok {
		return rpc.Response{Error: fmt.Sprintf("want {:mode ...}, got %s", req.Args)}
	}
	get := func(key string) puff.Value { return puff.Field(args, key) }
	kind, _ := get("mode").(puff.Keyword)
	services, _ := get("services").(puff.Vector)
	var m *mode
	switch kind {
	case "gated":
	case "free":
		m = &mode{free: true}
	case "trusted":
		m = &mode{trust: map[string]bool{}}
		for i := range services.Len() {
			k, ok := services.At(i).(puff.Keyword)
			if !ok {
				return rpc.Response{Error: fmt.Sprintf("want services like :todo, got %s", puff.Print(services.At(i)))}
			}
			m.trust[string(k)] = true
		}
		if len(m.trust) == 0 {
			return rpc.Response{Error: "trusted wants the services to trust"}
		}
	default:
		return rpc.Response{Error: fmt.Sprintf("want :gated, :trusted or :free, got %s", puff.Print(get("mode")))}
	}
	if f := get("for"); f != nil && m != nil {
		dur, ok := f.(puff.Duration)
		if !ok || dur <= 0 {
			return rpc.Response{Error: fmt.Sprintf("want :for as a duration like 2h, got %s", puff.Print(f))}
		}
		m.until = time.Now().Add(time.Duration(dur))
	}
	var res puff.Value
	var note string
	d.do(func(st *state) {
		if !st.isSession(addr) {
			res = puff.Errorf("no-address", "no agent session :%s", addr)
			return
		}
		for name := range m.trustSet() {
			if st.addrs[name] == nil {
				res = puff.Errorf("no-address", "no address :%s to trust", name)
				return
			}
		}
		st.setMode(addr, m)
		if m == nil {
			note = fmt.Sprintf(":%s is gated", addr)
		} else {
			note = fmt.Sprintf(":%s is %s", addr, m)
		}
		st.record(entry{at: time.Now(), note: note})
		res = puff.Done
	})
	return rpc.Response{Value: puff.Print(res)}
}

// isSession is whether addr is an agent session: one a client holds, or
// an agent's, :agent/<name>, while the agent :<name> is loaded, between
// its turns too, so its mode holds from one turn to the next.
func (st *state) isSession(addr string) bool {
	if !strings.HasPrefix(addr, "agent/") {
		return false
	}
	if s := st.addrs[addr]; s != nil {
		return s.client
	}
	return agentSession(addr) && st.addrs[strings.TrimPrefix(addr, "agent/")] != nil
}

// agentSession is whether addr is an agent's own session, :agent/<name>,
// rather than a whiff mcp client's :agent/<n>.
func agentSession(addr string) bool {
	name, ok := strings.CutPrefix(addr, "agent/")
	return ok && name != "" && strings.Trim(name, "0123456789") != ""
}

// trust adds service to the services the agent session addr is trusted
// for, making it trusted if it was gated, for as long as its mode lasts.
// A free session stays free.
func (st *state) trust(addr, service string) puff.Value {
	if !st.isSession(addr) {
		return puff.Errorf("no-address", "no agent session :%s to trust: it has gone", addr)
	}
	if st.addrs[service] == nil {
		return puff.Errorf("no-address", "no address :%s to trust", service)
	}
	old := st.modeOf(addr)
	if old != nil && old.free {
		return puff.Done
	}
	m := &mode{trust: map[string]bool{service: true}}
	if old != nil {
		maps.Copy(m.trust, old.trust)
		m.until = old.until
	}
	st.setMode(addr, m)
	st.record(entry{at: time.Now(), note: fmt.Sprintf(":%s is %s", addr, m)})
	return puff.Done
}

// trustSet is the services m trusts; none for a nil or free mode.
func (m *mode) trustSet() map[string]bool {
	if m == nil {
		return nil
	}
	return m.trust
}
