package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	eventually(t, "everything loads", func() string { return fmt.Sprint(strings.Count(list(), "\n") + 1) }, "7")
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
	}, "\n")
	eventually(t, "the services", list, want)

	// A file that goes is gone from the list, failed or not.
	os.Remove(filepath.Join(dir, "services", "broken.puff"))
	os.WriteFile(filepath.Join(dir, ".whiff", "agents", "thinker", "go"), nil, 0o600)
	eventually(t, "after broken.puff goes", func() string { return strings.SplitN(list(), "\n", 2)[0] }, `:crashy #f :faulting "restarted once today"`)
}
