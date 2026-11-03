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
)

// health is what whiff knows of a service file beyond its service: why
// the file last failed to load, if it did, and when the service faulted.
type health struct {
	loadErr string      // "" once it loads
	faults  []time.Time // the last 24 hours'
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
	d.do(func(st *state) { st.healthOf(serviceName(file)).loadErr = msg })
}

func (d *daemon) loadedOK(file string) {
	d.do(func(st *state) { st.healthOf(serviceName(file)).loadErr = "" })
}

// faulted keeps that s faulted now.
func (st *state) faulted(s *service) {
	if s.client || !s.keep {
		return
	}
	h := st.healthOf(s.name)
	since := time.Now().Add(-24 * time.Hour)
	h.faults = append(slices.DeleteFunc(h.faults, func(t time.Time) bool { return t.Before(since) }), time.Now())
}

func serviceName(file string) string { return strings.TrimSuffix(filepath.Base(file), ".puff") }

// status is a service file's line in whiff services: one state word and
// at most one note, the thing most worth knowing.
type status struct {
	name  string
	agent bool
	state string // ok, failed, faulting, over-budget, waiting, thinking, busy, idle
	note  string
}

// statusRank orders whiff services: what needs you first, what runs next.
var statusRank = map[string]int{"failed": 0, "faulting": 1, "over-budget": 2, "waiting": 3, "thinking": 4, "busy": 4}

// statuses are the service files' statuses, those that need you first.
func (st *state) statuses(now time.Time) []status {
	names := map[string]bool{}
	for name, s := range st.addrs {
		if !s.client && s.keep && name != "jobs" && name != "me" && name != "review" {
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
	answers, reviews := 0, 0
	for _, a := range st.inbox("me") {
		if a.from == name {
			answers++
		}
	}
	for _, a := range st.inbox("review") {
		if a.from == name || a.from == "agent/"+name {
			reviews++
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
	case r.agent && s.dayUSD > 0 && agentSpent(name, now) >= s.dayUSD:
		r.state, r.note = "over-budget", fmt.Sprintf("$%.2f of its $%.2f a day", agentSpent(name, now), s.dayUSD)
	case answers > 0 || reviews > 0:
		r.state = "waiting"
		var parts []string
		if answers > 0 {
			one, many := "message", "messages"
			if r.agent {
				one, many = "answer", "answers"
			}
			parts = append(parts, plural(answers, one, many)+" in your inbox")
		}
		if reviews > 0 {
			parts = append(parts, plural(reviews, "write waits", "writes wait")+" for review")
		}
		r.note = strings.Join(parts, ", ")
	case len(running) > 0 && r.agent:
		r.state = "thinking"
		if a := running[0].attempts; len(a) > 0 {
			r.note = now.Sub(a[len(a)-1].started).Round(time.Second).String()
		}
		if q := len(st.inbox(name)) - 1; q > 0 {
			r.note += fmt.Sprintf(", %d queued", q)
		}
	case len(running) > 0:
		r.state, r.note = "busy", plural(len(running), "job", "jobs")
	case r.agent:
		r.state = "idle"
	default:
		r.state = "ok"
	}
	return r
}

// agentSpent is the dollars agent name's turns cost in the 24 hours
// before now, from the costs file its turn loop keeps.
func agentSpent(name string, now time.Time) float64 {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	b, _ := os.ReadFile(filepath.Join(home, ".whiff", "agents", name, "costs"))
	sum := 0.0
	for _, line := range strings.Split(string(b), "\n") {
		at, usd, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		f, ferr := strconv.ParseFloat(usd, 64)
		if err == nil && ferr == nil && now.Sub(t) < 24*time.Hour {
			sum += f
		}
	}
	return sum
}

func (s status) value() puff.Value {
	m, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("service"), Val: puff.Keyword(s.name)},
		puff.Entry{Key: puff.Keyword("agent"), Val: puff.Bool(s.agent)},
		puff.Entry{Key: puff.Keyword("status"), Val: puff.Keyword(s.state)},
		puff.Entry{Key: puff.Keyword("note"), Val: puff.String(s.note)},
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
