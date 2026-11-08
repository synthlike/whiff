package daemon

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"go.etcd.io/bbolt"
)

// health is what whiff knows of a service file beyond its service: its
// path, why it last failed to load, if it did, when the service faulted
// and the last message it handled.
type health struct {
	file    string
	loadErr string      // "" once it loads
	faults  []time.Time // the last 24 hours'
	fault   string      // the last fault's err
	last    handled     // the zero handled before it handles one
}

// handled is a message a service handled: its function, or topic, and
// when.
type handled struct {
	fn string
	at time.Time
}

// faultWindow is how long a fault keeps a service :faulting.
const faultWindow = time.Hour

func (st *state) healthOf(name string) *health {
	h := st.health[name]
	if h == nil {
		h = &health{}
		st.health[name] = h
	}
	return h
}

// loadFailed and loadedOK keep how file's last load went.
func (d *daemon) loadFailed(file string, err error) {
	// Its file is in the services directory: say only its name.
	msg := strings.ReplaceAll(err.Error(), filepath.Dir(file)+string(filepath.Separator), "")
	d.do(func(st *state) {
		h := st.healthOf(serviceName(file))
		h.file, h.loadErr = file, msg
	})
}

func (d *daemon) loadedOK(file string) {
	d.do(func(st *state) {
		h := st.healthOf(serviceName(file))
		h.file, h.loadErr = file, ""
	})
}

// served keeps that s handled m, at begin.
func (st *state) served(s *service, m message, begin time.Time) {
	if s.client || !s.keep {
		return
	}
	fn := m.fn
	if m.topic != "" {
		fn = ":" + m.topic
	}
	st.healthOf(s.name).last = handled{fn, begin}
}

// faulted keeps that s faulted now, with the err it replied.
func (st *state) faulted(s *service, reply string) {
	if s.client || !s.keep {
		return
	}
	h := st.healthOf(s.name)
	h.fault = reply
	// Its message, at a place in its file, which is in the services
	// directory: say only the file's name.
	if v, err := puff.ReadValue(reply); err == nil {
		if t, ok := v.(puff.Tag); ok && t.Len() == 1 {
			if e, ok := t.At(0).(puff.Map); ok {
				if msg, ok := e.Get(puff.Keyword("message")); ok {
					if text, ok := msg.(puff.String); ok {
						h.fault = string(text)
					}
				}
			}
		}
	}
	if h.file != "" {
		h.fault = strings.ReplaceAll(h.fault, filepath.Dir(h.file)+string(filepath.Separator), "")
	}
	since := time.Now().Add(-24 * time.Hour)
	h.faults = append(slices.DeleteFunc(h.faults, func(t time.Time) bool { return t.Before(since) }), time.Now())
}

func serviceName(file string) string { return strings.TrimSuffix(filepath.Base(file), ".puff") }

// status is a service file's line in whiff services: one state word and
// at most one note, the thing most worth knowing.
type status struct {
	name  string
	agent bool
	state string // ok, failed, faulting, over-budget, paused, waiting, thinking, busy, idle
	note  string
}

// statusRank orders whiff services: what needs you first, what runs next.
var statusRank = map[string]int{"failed": 0, "faulting": 1, "over-budget": 2, "paused": 3, "waiting": 3, "thinking": 4, "busy": 4}

// statuses are the service files' statuses, those that need you first.
func (st *state) statuses(now time.Time) []status {
	names := map[string]bool{}
	for name, s := range st.addrs {
		if !s.client && s.keep && !own(name) {
			names[name] = true
		}
	}
	for name, h := range st.health {
		if h.loadErr != "" {
			names[name] = true
		}
	}
	var all []status
	for _, name := range slices.Sorted(maps.Keys(names)) {
		all = append(all, st.statusOf(name, now))
	}
	rank := func(s status) int {
		if r, ok := statusRank[s.state]; ok {
			return r
		}
		return 5
	}
	slices.SortStableFunc(all, func(a, b status) int { return rank(a) - rank(b) })
	return all
}

func (st *state) statusOf(name string, now time.Time) status {
	s := st.addrs[name]
	h := st.health[name]
	if h == nil {
		h = &health{}
	}
	r := status{name: name, agent: s != nil && s.agent}
	recent := 0
	for _, t := range h.faults {
		if now.Sub(t) < faultWindow {
			recent++
		}
	}
	// What waits for you: an agent's questions, and its reports, which
	// wait for nothing but your reading them; a service's messages, either.
	// An agent asks as itself or from its session, agent/name.
	ours := func(from string) bool { return from == name || from == "agent/"+name }
	answers, questions, reports, reviews := 0, 0, 0, 0
	for _, a := range st.inbox("me") {
		if ours(a.from) {
			answers++
			if a.fn == "tell" {
				reports++
			} else {
				questions++
			}
		}
	}
	for _, a := range st.inbox("review") {
		if ours(a.from) {
			reviews++
		}
	}
	// children are the tasks an agent's session handed out that haven't
	// come back, the oldest first, as st.asked is.
	var children []*asked
	for _, a := range st.asked {
		if r.agent && a.from == "agent/"+name && a.held == nil && a.addr != "me" && a.addr != "review" {
			children = append(children, a)
		}
	}
	var running []*job
	for _, j := range st.jobs {
		if j.from == name {
			running = append(running, j)
		}
	}
	switch {
	case h.loadErr != "":
		r.state, r.note = "failed", firstLine(h.loadErr)
		if s != nil {
			r.note += "; the last version runs"
		}
	case recent > 0:
		r.state, r.note = "faulting", fmt.Sprintf("restarted %s today", times(len(h.faults)))
	case r.agent && s.dayUSD > 0 && agentSpent(name, now).usd >= s.dayUSD:
		r.state, r.note = "over-budget", fmt.Sprintf("%s of its %s a day", usd(agentSpent(name, now).usd), usd(s.dayUSD))
	case r.agent && agentPaused(name):
		r.state = "paused"
		if q := len(st.inbox(name)); q > 0 {
			r.note = fmt.Sprintf("%d queued", q)
		}
	case !r.agent && (answers > 0 || reviews > 0):
		r.state = "waiting"
		var parts []string
		if answers > 0 {
			parts = append(parts, plural(answers, "message", "messages")+" in your inbox")
		}
		if reviews > 0 {
			parts = append(parts, plural(reviews, "write waits", "writes wait")+" for review")
		}
		r.note = strings.Join(parts, ", ")
	case r.agent && (questions > 0 || reviews > 0):
		r.state = "waiting"
		var parts []string
		if questions > 0 {
			parts = append(parts, "for your answer to "+plural(questions, "question", "questions"))
		}
		if reviews > 0 {
			parts = append(parts, "for your review of "+plural(reviews, "write", "writes"))
		}
		r.note = strings.Join(parts, " and ")
	case len(running) > 0 && r.agent:
		r.state = "thinking"
		if a := running[0].attempts; len(a) > 0 {
			r.note = now.Sub(a[len(a)-1].started).Round(time.Second).String()
			// A turn that failed and runs again says so.
			if len(a) > 1 {
				r.note += fmt.Sprintf(", attempt %d of %d", len(a), running[0].opts.retry+1)
			}
		}
		if q := len(st.inbox(name)) - 1; q > 0 {
			r.note += fmt.Sprintf(", %d queued", q)
		}
	case len(running) > 0:
		r.state, r.note = "busy", plural(len(running), "job", "jobs")
	case len(children) > 0:
		// Its turns have ended, but what they handed out hasn't come back.
		r.state, r.note = "busy", "waiting on "+waitedOn(children, now)
	case r.agent:
		r.state = "idle"
	default:
		r.state = "ok"
	}
	// A report waits for nothing, so it is said by the way, after
	// whatever an agent is doing; not after what is wrong with it.
	if r.agent && reports > 0 && (r.state == "idle" || r.state == "busy" || r.state == "thinking") {
		r.note = strings.TrimPrefix(r.note+"; "+plural(reports, "report", "reports")+" in your inbox", "; ")
	}
	return r
}

// waitedOn says what an agent's open tasks wait on, oldest first: the
// oldest's address, task id and how long, and how many more,
// ":pipeline (task 178) for 4m, and 1 more".
func waitedOn(children []*asked, now time.Time) string {
	a := children[0]
	s := fmt.Sprintf(":%s (task %d) for %s", a.addr, a.id, now.Sub(a.at).Round(time.Second))
	if n := len(children) - 1; n > 0 {
		s += fmt.Sprintf(", and %d more", n)
	}
	return s
}

// agentPaused is whether agent name is paused, by the file its turn loop
// keeps.
func agentPaused(name string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	b, _ := os.ReadFile(filepath.Join(home, ".whiff", "agents", name, "paused"))
	return strings.TrimSpace(string(b)) == "paused"
}

// spending is what an agent spent in the last 24 hours, its turns and
// the tasks they started whose results said what they cost: dollars, how
// many turns, and when the last ended.
type spending struct {
	usd   float64
	tasks float64 // of usd, what its tasks cost, as a pipeline build's dev
	turns int
	last  time.Time
}

// agentSpent is agent name's spending in the 24 hours before now, from
// the costs file its turn loop keeps.
func agentSpent(name string, now time.Time) spending {
	var sp spending
	home, err := os.UserHomeDir()
	if err != nil {
		return sp
	}
	b, _ := os.ReadFile(filepath.Join(home, ".whiff", "agents", name, "costs"))
	// Each line is "time usd" for a turn, "time usd - project" for one in
	// a project, "time usd task" for a task.
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f[0])
		usd, ferr := strconv.ParseFloat(f[1], 64)
		if err != nil || ferr != nil || now.Sub(t) >= 24*time.Hour {
			continue
		}
		sp.usd += usd
		if len(f) == 2 || f[2] == "-" {
			sp.turns, sp.last = sp.turns+1, t
		} else {
			sp.tasks += usd
		}
	}
	return sp
}

// serviceStatus is service/status name: its line of whiff services, and
// what else whiff knows of it, as a map whose keys are there only when
// they have something to say.
func (d *daemon) serviceStatus(name string) puff.Value {
	now := time.Now()
	var es []puff.Entry
	add := func(k string, v puff.Value) { es = append(es, puff.Key(k, v)) }
	found := false
	var agent bool
	var day float64
	d.do(func(st *state) {
		s, h := st.addrs[name], st.health[name]
		if s == nil && (h == nil || h.loadErr == "") || s != nil && (s.client || !s.keep) {
			return
		}
		found = true
		if h == nil {
			h = &health{}
		}
		r := st.statusOf(name, now)
		agent = r.agent
		add("service", puff.Keyword(name))
		add("agent", puff.Bool(r.agent))
		add("status", puff.Keyword(r.state))
		add("note", puff.String(r.note))
		if h.file != "" {
			add("file", puff.String(h.file))
		}
		if h.loadErr != "" {
			add("load-error", puff.String(h.loadErr))
		}
		if !h.last.at.IsZero() {
			add("last", mapOf("fn", puff.Keyword(h.last.fn), "at", puff.NewTime(h.last.at)))
		}
		if len(h.faults) > 0 {
			add("faults", puff.Int(len(h.faults)))
			add("fault", mapOf("at", puff.NewTime(h.faults[len(h.faults)-1]), "message", puff.String(h.fault)))
		}
		add("inbox", puff.Int(len(st.inbox(name))))
		if s != nil && s.agent {
			day = s.dayUSD
			if m := st.modeOf("agent/" + name); m != nil {
				add("mode", m.value())
			}
			if s.every > 0 {
				add("every", puff.Duration(s.every))
			}
		}
	})
	if !found {
		return puff.Errorf("no-address", "no service :%s", name)
	}
	var timers []puff.Value
	d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(timersBucket).ForEach(func(k, v []byte) error {
			if t, err := storedTimer(slices.Clone(k), string(v)); err == nil && t.addr == name {
				fn := t.m.fn
				if t.m.topic != "" {
					fn = ":" + t.m.topic
				}
				timers = append(timers, mapOf("fn", puff.Keyword(fn), "due", puff.NewTime(t.due)))
			}
			return nil
		})
	})
	add("timers", puff.NewVector(timers...))
	if agent {
		sp := agentSpent(name, now)
		add("spent", puff.Float(sp.usd))
		add("tasks-spent", puff.Float(sp.tasks))
		add("turns", puff.Int(sp.turns))
		add("day", puff.Float(day))
		if !sp.last.IsZero() {
			add("last-turn", puff.NewTime(sp.last))
		}
		add("work", d.agentWork(name, now))
	}
	m, err := puff.NewMap(es...)
	if err != nil {
		return puff.Errorf("bad-call", "%v", err)
	}
	return puff.Ok(m)
}

// mapOf is a map of keyword keys and values, alternating.
func mapOf(kvs ...any) puff.Value {
	var es []puff.Entry
	for i := 0; i+1 < len(kvs); i += 2 {
		es = append(es, puff.Key(kvs[i].(string), kvs[i+1].(puff.Value)))
	}
	m, _ := puff.NewMap(es...)
	return m
}

func (s status) value() puff.Value {
	m, _ := puff.NewMap(
		puff.Key("service", puff.Keyword(s.name)),
		puff.Key("agent", puff.Bool(s.agent)),
		puff.Key("status", puff.Keyword(s.state)),
		puff.Key("note", puff.String(s.note)),
	)
	return m
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// usd is dollars x to the cent, as $0.42; "under $0.01" for a spend that
// rounds to nothing, as a cheap model's turn does.
func usd(x float64) string {
	if x > 0 && x < 0.005 {
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", x)
}
