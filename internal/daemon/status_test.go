package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// TestServiceList has a service file in each state service/list knows,
// and checks the order whiff services shows them in: what needs you
// first, what runs next, then the rest, each by name.
func TestServiceList(t *testing.T) {
	dir := tempDir(t)
	fakeClaude(t, dir)
	writeServices(t, dir, map[string]string{
		"broken":  `(service broken (on (x) "x" (ok 1))`,
		"crashy":  `(service crashy (on (x) "x" (ok (unwrap (none)))))`,
		"spender": `(agent spender :prompt "Spend." :limits {:day 0.01})`,
		"thinker": `(agent thinker :prompt "Think.")`,
		"teller":  `(service teller (on (x) "x" (call :me/tell "hi")))`,
		"calm":    `(service calm (on (x) "x" (ok 1)))`,
		"sleepy":  `(agent sleepy :prompt "Sleep.")`,
		"ticker":  `(service ticker :start (fn () (do (send-after 1h :tick) (ok :ticking))) (on (tick) "t" (ok 1)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	list := func() string {
		r, err := c.Eval("(service/list)", "")
		if err != nil || r.Error != "" {
			return "error: " + r.Error
		}
		v, _ := puff.ReadValue(r.Value)
		all, _ := v.(puff.Tag).At(0).(puff.Vector)
		var lines []string
		for i := range all.Len() {
			m := all.At(i).(puff.Map)
			get := func(k string) string { v, _ := m.Get(puff.Keyword(k)); return puff.Print(v) }
			note := get("note")
			if get("service") == ":thinker" && strings.HasSuffix(note, `s"`) {
				note = "<time>" // how long it has thought
			}
			lines = append(lines, get("service")+" "+get("agent")+" "+get("status")+" "+note)
		}
		return strings.Join(lines, "\n")
	}
	eventually(t, "everything loads", func() string { return fmt.Sprint(strings.Count(list(), "\n") + 1) }, "8")
	for _, call := range []string{`(call :crashy/x)`, `(call :teller/x)`, `(call :spender/tell "spend it")`, `(call :thinker/tell "hold")`} {
		c.Eval(call, "")
	}
	want := strings.Join([]string{
		`:broken #f :failed "broken.puff:1:1: no closing ')'"`,
		`:crashy #f :faulting "restarted once today"`,
		`:spender #t :over-budget "$0.01 of its $0.01 a day"`,
		`:teller #f :waiting "1 message in your inbox"`,
		`:thinker #t :thinking <time>`,
		`:calm #f :ok ""`,
		`:sleepy #t :idle ""`,
		`:ticker #f :ok ""`,
	}, "\n")
	eventually(t, "the services", list, want)

	// service/status has the rest: each key only with something to say.
	status := func(name string) puff.Map {
		r, err := c.Eval("(service/status :"+name+")", "")
		v, _ := puff.ReadValue(r.Value)
		m, ok := v.(puff.Tag).At(0).(puff.Map)
		if err != nil || !ok {
			t.Fatalf("service/status :%s = %+v %v", name, r, err)
		}
		return m
	}
	has := func(name string, want ...string) {
		t.Helper()
		got := puff.Print(status(name))
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("service/status :%s = %s\nwant it to have %s", name, got, w)
			}
		}
	}
	has("teller", ":last {:at ", ":fn :x}", `:file "`+filepath.Join(dir, "services", "teller.puff")+`"`, ":inbox 0", ":timers []")
	has("crashy", ":faults 1", `:fault {:at `, `:message "crashy.puff:1:33: unwrap: got (none)"}`)
	has("broken", `:load-error "broken.puff:1:1: no closing ')'"`)
	has("ticker", ":timers [{:due ", ":fn :tick}]")
	has("spender", ":turns 1", ":spent 0.01", ":day 0.01", ":last-turn ")
	if got := puff.Print(status("calm")); strings.Contains(got, ":last ") || strings.Contains(got, ":fault") || strings.Contains(got, ":spent") {
		t.Errorf("service/status :calm = %s, want no :last, :fault or :spent", got)
	}
	if r, _ := c.Eval("(service/status :me)", ""); !strings.HasPrefix(r.Value, "(err {:kind :no-address") {
		t.Errorf("service/status :me = %+v, want whiff's own address unknown", r)
	}

	// A file that goes is gone from the list, failed or not.
	os.Remove(filepath.Join(dir, "services", "broken.puff"))
	os.WriteFile(filepath.Join(dir, ".whiff", "agents", "thinker", "go"), nil, 0o600)
	eventually(t, "after broken.puff goes", func() string { return strings.SplitN(list(), "\n", 2)[0] }, `:crashy #f :faulting "restarted once today"`)
}

// TestAgentWaits checks what an agent's line says it waits on: your
// answer or review, which need you; a child's task, or a turn running
// again, which don't; and a report, which waits for nothing.
func TestAgentWaits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	ask := func(id int64, from, addr, fn string, held bool) *asked {
		a := &asked{ticket: ticket{id: id, from: from}, addr: addr, fn: fn, at: now.Add(-4 * time.Minute)}
		if held {
			a.held, a.to = &message{}, "todo"
		}
		return a
	}
	turn := &job{ticket: ticket{from: "lead"}, opts: jobOpts{retry: 2},
		attempts: []attempt{{started: now.Add(-time.Minute)}, {started: now.Add(-3 * time.Second)}}}
	cases := []struct {
		name  string
		asked []*asked
		jobs  []*job
		want  string
	}{
		{"nothing", nil, nil, `idle ""`},
		{"a report", []*asked{ask(1, "lead", "me", "tell", false)}, nil, `idle "1 report in your inbox"`},
		{"a question", []*asked{ask(1, "agent/lead", "me", "ask", false), ask(2, "lead", "me", "tell", false)}, nil,
			`waiting "for your answer to 1 question"`},
		{"a review", []*asked{ask(1, "agent/lead", "review", "", true), ask(2, "agent/lead", "me", "ask", false)}, nil,
			`waiting "for your answer to 1 question and for your review of 1 write"`},
		{"children", []*asked{ask(178, "agent/lead", "pipeline", "run", false), ask(180, "agent/lead", "pipeline", "run", false),
			ask(3, "lead", "me", "tell", false)}, nil,
			`busy "waiting on :pipeline (task 178) for 4m0s, and 1 more; 1 report in your inbox"`},
		{"a retried turn", []*asked{ask(178, "agent/lead", "pipeline", "run", false)}, []*job{turn}, `thinking "3s, attempt 2 of 3"`},
		// Another's task isn't lead's to wait on.
		{"another's", []*asked{ask(1, "agent/digest", "pipeline", "run", false), ask(2, "digest", "me", "ask", false)}, nil, `idle ""`},
	}
	for _, c := range cases {
		st := &state{addrs: map[string]*service{"lead": {name: "lead", agent: true}}, asked: c.asked,
			jobs: map[int64]*job{}, health: map[string]*health{}}
		for i, j := range c.jobs {
			st.jobs[int64(i)] = j
		}
		r := st.statusOf("lead", now)
		if got := fmt.Sprintf("%s %q", r.state, r.note); got != c.want {
			t.Errorf("%s: lead is %s, want %s", c.name, got, c.want)
		}
	}
}

// Dollars show to the cent, and a spend that rounds to nothing, as a
// cheap model's turn, as under a cent rather than $0.00.
func TestUSD(t *testing.T) {
	t.Parallel()
	for x, want := range map[float64]string{0: "$0.00", 0.003: "under $0.01", 0.005: "$0.01", 1.2: "$1.20"} {
		if got := usd(x); got != want {
			t.Errorf("usd(%v) = %q, want %q", x, got, want)
		}
	}
}
