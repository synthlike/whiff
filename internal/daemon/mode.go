package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// Modes: how the review gate treats an agent session, an :agent/<n>
// address. Gated, the default, holds its :write messages. Trusted lets its
// messages to the services you name through as if you had accepted them,
// so what those services do for it goes through too. Free lets everything
// it sends, publishes and evaluates through. Only you set a mode, as only
// you accept; it lasts until the session ends, or for a while.

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
		delete(st.modes, addr)
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

// modeValue is addr's mode as (mode) gives it: (ok {:mode ...}), or (err
// {:kind :no-session}) if addr isn't an agent session.
func (st *state) modeValue(addr string) puff.Value {
	if s := st.addrs[addr]; s == nil || !s.client || !strings.HasPrefix(addr, "agent/") {
		return puff.Errorf("no-session", "only an agent session has a mode")
	}
	es := []puff.Entry{{Key: puff.Keyword("mode"), Val: puff.Keyword("gated")}}
	if m := st.modeOf(addr); m != nil {
		es[0].Val = puff.Keyword(m.name())
		if !m.free {
			var services []puff.Value
			for _, name := range slices.Sorted(maps.Keys(m.trust)) {
				services = append(services, puff.Keyword(name))
			}
			es = append(es, puff.Entry{Key: puff.Keyword("services"), Val: puff.NewVector(services...)})
		}
		if !m.until.IsZero() {
			es = append(es, puff.Entry{Key: puff.Keyword("until"), Val: puff.NewTime(m.until)})
		}
	}
	v, _ := puff.NewMap(es...)
	return puff.Ok(v)
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
	get := func(key string) puff.Value {
		v, _ := args.Get(puff.Keyword(key))
		return v
	}
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
		s := st.addrs[addr]
		if s == nil || !s.client || !strings.HasPrefix(addr, "agent/") {
			res = puff.Errorf("no-address", "no agent session :%s", addr)
			return
		}
		for name := range m.trustSet() {
			if st.addrs[name] == nil {
				res = puff.Errorf("no-address", "no address :%s to trust", name)
				return
			}
		}
		if m == nil {
			delete(st.modes, addr)
			note = fmt.Sprintf(":%s is gated", addr)
		} else {
			st.modes[addr] = m
			note = fmt.Sprintf(":%s is %s", addr, m)
		}
		st.record(entry{at: time.Now(), note: note})
		res = puff.Done
	})
	return rpc.Response{Value: puff.Print(res)}
}

// trustSet is the services m trusts; none for a nil or free mode.
func (m *mode) trustSet() map[string]bool {
	if m == nil {
		return nil
	}
	return m.trust
}
