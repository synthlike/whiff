package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// TestTasks shows each kind of task in :tasks, held, waiting, running and
// ended, with who owns it and what can be done with it, and cancels one.
func TestTasks(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"box":   `(service box (ask (q x) "Waits."))`,
		"notes": `(service notes (on (add text) :write "Adds text." (ok :done)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true
	watch := dial(t, dir)
	if _, err := watch.Sub("review/pending", "tasks/changed"); err != nil {
		t.Fatal(err)
	}

	call := func(target, args string) string {
		t.Helper()
		resp, err := c.Message("call", target, args, dir)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value
	}
	id := func(r string) string {
		t.Helper()
		v, _ := puff.ReadValue(r)
		n, ok := pendingID(v)
		if !ok {
			t.Fatalf("%s isn't pending", r)
		}
		return puff.Print(puff.Int(n))
	}
	// tasks is tasks/list, each task's fields printed, by id.
	tasks := func() map[string]map[string]string {
		t.Helper()
		v, err := puff.ReadValue(call("tasks/list", "[]"))
		if err != nil {
			t.Fatal(err)
		}
		list := v.(puff.Tag).At(0).(puff.Vector)
		got := map[string]map[string]string{}
		for i := range list.Len() {
			m := list.At(i).(puff.Map)
			fs := map[string]string{}
			for j := range m.Len() {
				e := m.Entry(j)
				fs[string(e.Key.(puff.Keyword))] = puff.Print(e.Val)
			}
			got[fs["id"]] = fs
		}
		return got
	}

	waiting := id(call("box/q", "[1]"))
	failing := id(call("box/q", "[2]"))
	resp, err := agent.Message("call", "notes/add", `["x"]`, dir)
	if err != nil {
		t.Fatal(err)
	}
	held := id(resp.Value)
	job := id(call("jobs/run", `[["sleep" "10"]]`))
	// An ended task offers the actions its result does, beside Result.
	if r, _ := c.Message("reply", "", "["+failing+` (err {:kind :boom :actions [{:label "Again" :action [:box/q 2]}]})]`, dir); r.Value != "(ok :done)" {
		t.Fatalf("reply = %s", r.Value)
	}

	got := tasks()
	for _, tt := range []struct {
		id                      string
		status, state, what, to string
	}{
		{waiting, ":waiting", ":waiting", `":box/q 1"`, ":box"},
		{held, ":held", ":held", `":notes/add \"x\""`, ":notes"},
		{job, ":running", ":running", `"sleep 10"`, ":jobs"},
		// You answered it, as a client: nobody's :to.
		{failing, ":ended", ":failed", `":box/q 2"`, ""},
	} {
		fs := got[tt.id]
		if fs["status"] != tt.status || fs["state"] != tt.state || fs["what"] != tt.what || fs["to"] != tt.to {
			t.Errorf("task %s = %v, want %s %s %s %s", tt.id, fs, tt.status, tt.state, tt.what, tt.to)
		}
	}
	if r := got[failing]["result"]; r != "(err {:kind :boom})" {
		t.Errorf("the failed task's result, cut short = %s", r)
	}

	view := call("tasks/view", "[]")
	for _, want := range []string{
		`{:actions [{:action [:tasks/cancel ` + waiting + `] :label "Cancel"}] :detail "task ` + waiting + ` for you · just now" :label "waiting  :box/q 1"}`,
		`{:action [:review/accept ` + held + `] :label "Accept"}`,
		`{:actions [{:action [:jobs/output ` + job + `] :label "Output"} {:action [:tasks/cancel ` + job + `] :label "Cancel"}]`,
		`{:actions [{:action [:tasks/result ` + failing + `] :label "Result"} {:action [:box/q 2] :label "Again"}] :detail "task ` + failing + ` for you · just now · (err {:kind :boom})" :label "failed  :box/q 2"}`,
		`{:changed :tasks/changed}`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("tasks/view lacks %s:\n%s", want, view)
		}
	}
	if r := call("tasks/result", "["+failing+"]"); !strings.HasPrefix(r, "(ok (err {") || !strings.Contains(r, ":kind :boom") {
		t.Errorf("tasks/result = %s", r)
	}
	if r := call("tasks/result", "["+waiting+"]"); !strings.Contains(r, ":no-id") {
		t.Errorf("tasks/result of one waiting = %s", r)
	}

	// Cancel is yours: an agent can't, even its own; you can.
	if r, _ := agent.Message("call", "tasks/cancel", "["+held+"]", dir); !strings.HasPrefix(r.Value, "(err") {
		t.Errorf("agent's tasks/cancel = %s", r.Value)
	}
	if r := call("tasks/cancel", "["+job+"]"); r != "(ok :done)" {
		t.Errorf("tasks/cancel = %s", r)
	}
	eventually(t, "the cancelled job's state", func() string { return tasks()[job]["state"] }, ":cancelled")
	if w := tasks()[job]["what"]; w != `"sleep 10"` {
		t.Errorf("the ended job's what = %s", w)
	}
	// Failed shows only the one that failed: not those waiting, nor the job
	// you cancelled.
	if v := call("tasks/failed", "[]"); !strings.Contains(v, `:label "failed  :box/q 2"`) || strings.Count(v, ":detail") != 1 {
		t.Errorf("tasks/failed = %s", v)
	}

	// A job with :retry says how many times it ran, of how many it may.
	timed := id(call("jobs/run", `[["sleep" "10"] {:timeout 100ms :retry 1 :idempotent #t}]`))
	eventually(t, "the timed-out job's attempts", func() string {
		fs := tasks()[timed]
		return fs["state"] + " " + fs["attempts"] + " of " + fs["tries"]
	}, ":failed 2 of 2")
	if v := call("tasks/failed", "[]"); !strings.Contains(v, `task `+timed+` for you · by :jobs · just now · 2 attempts · (err {:kind :timeout`) {
		t.Errorf("tasks/failed with the timed-out job = %s", v)
	}

	// Each task that began or ended was published on :tasks/changed.
	seen := map[string]bool{}
	for deadline := time.After(2 * time.Second); !seen[waiting] || !seen[failing] || !seen[held] || !seen[job]; {
		select {
		case e := <-watch.Events():
			if e.Topic == "tasks/changed" {
				seen[e.Value] = true
			}
		case <-deadline:
			t.Fatalf(":tasks/changed for %v, want %s %s %s %s", seen, waiting, failing, held, job)
		}
	}
}

// TestTaskTree follows a task's work down: an ask whose whiff/asked runs
// a job, as pipeline's build its dev, whose session asks another service,
// as a dev its lead. Each is the child of the one whose work began it, and
// tasks/tree gives them nested.
func TestTaskTree(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"boss": `(service boss
  (ask (build x) "Builds x.")
  (on (whiff/asked id) "Runs a worker for it." (call :jobs/run ["sh" "-c" "echo $WHIFF_SESSION > token; while [ ! -e done ]; do sleep 0.02; done"] {:session [:helper/ask]}))
  (on (whiff/done id r) "Hears the worker ended." (ok :done)))`,
		"helper": `(service helper (ask (ask q) "Asks the helper."))`,
	})
	start(t, dir)
	c := dial(t, dir)
	r, _ := c.Message("call", "boss/build", `["x"]`, dir)
	v, _ := puff.ReadValue(r.Value)
	n, ok := pendingID(v)
	if !ok {
		t.Fatalf("build = %s", r.Value)
	}
	build := puff.Print(puff.Int(n))
	var tok string
	eventually(t, "the worker's token", func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "token"))
		tok = strings.TrimSpace(string(b))
		return fmt.Sprint(len(tok))
	}, "32")
	worker := dial(t, dir)
	worker.Agent = true
	if r, err := worker.ClaimSession(tok, ""); err != nil || !strings.HasPrefix(r.Value, "(ok [:agent/") {
		t.Fatalf("the worker's claim = %+v %v", r, err)
	}
	r, _ = worker.Message("call", "helper/ask", `["which port?"]`, "")
	v, _ = puff.ReadValue(r.Value)
	q, ok := pendingID(v)
	if !ok {
		t.Fatalf("the worker's ask = %s", r.Value)
	}

	resp, _ := c.Message("call", "tasks/tree", "["+build+"]", dir)
	tree := resp.Value
	// build > the worker's job > its ask of helper.
	re := regexp.MustCompile(`^\(ok \{:at \S+ :began \S+ :children \[\{:at \S+ :began \S+ :children \[\{:at \S+ :began \S+ :children \[\] :from :agent/\d+ :id ` + fmt.Sprint(q) + ` :parent (\d+) .*:what ":helper/ask \\"which port\?\\""\}\] :cmd \["sh" .*:from :boss :id (\d+) :parent ` + build + ` .*:status :running .*\}\] :id ` + build + ` .*:what ":boss/build \\"x\\""\}\)$`)
	m := re.FindStringSubmatch(tree)
	if m == nil || m[1] != m[2] {
		t.Fatalf("tasks/tree %s = %s", build, tree)
	}
	// Once the worker's job ends, the tree keeps it, ended.
	os.WriteFile(filepath.Join(dir, "done"), nil, 0o600)
	eventually(t, "the worker's job, ended", func() string {
		resp, _ := c.Message("call", "tasks/tree", "["+build+"]", dir)
		return fmt.Sprint(strings.Contains(resp.Value, ":id "+m[2]+" :parent "+build) && strings.Contains(resp.Value, ":status :ended"))
	}, "true")
	if resp, _ := c.Message("call", "tasks/tree", "[99999]", dir); !strings.Contains(resp.Value, ":no-id") {
		t.Errorf("tasks/tree of none = %s", resp.Value)
	}
}
