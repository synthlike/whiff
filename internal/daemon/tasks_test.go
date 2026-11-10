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
	t.Parallel()
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
	for id, want := range map[string]string{waiting: `"waits for :box to reply"`, held: `"waits for your review"`} {
		if got[id]["why"] != want {
			t.Errorf("task %s's why = %s, want %s", id, got[id]["why"], want)
		}
	}
	if r := got[failing]["result"]; r != "(err {:kind :boom})" {
		t.Errorf("the failed task's result, cut short = %s", r)
	}

	told := id(call("me/tell", `["done"]`))
	view := call("tasks/view", "[]")
	// One in your inbox is answered or dismissed, as Inbox has it, not
	// cancelled.
	if !strings.Contains(view, `{:actions [{:action [:me/answer `+told+`] :label "Dismiss"}] :detail "task `+told+` for you · just now · waits for you to read it"`) || strings.Contains(view, "[:tasks/cancel "+told+"]") {
		t.Errorf("tasks/view's report in your inbox:\n%s", view)
	}
	for _, want := range []string{
		`{:actions [{:action [:tasks/cancel ` + waiting + `] :label "Cancel"}] :detail "task ` + waiting + ` for you · just now · waits for :box to reply" :label "waiting  :box/q 1"}`,
		`{:action [:review/accept ` + held + `] :label "Accept"}`,
		`{:actions [{:action [:tasks/watch ` + job + `] :label "Watch"} {:action [:tasks/cancel ` + job + `] :label "Cancel"}]`,
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
	t.Parallel()
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
	re := regexp.MustCompile(`^\(ok \{:at \S+ :began \S+ :children \[\{:at \S+ :began \S+ :children \[\{:at \S+ :began \S+ :children \[\] :from :agent/\d+ :id ` + fmt.Sprint(q) + ` :parent (\d+) .*:what ":helper/ask \\"which port\?\\"" :why "[^"]*"\}\] :cmd \["sh" .*:from :boss :id (\d+) :parent ` + build + ` .*:status :running .*\}\] :id ` + build + ` .*:what ":boss/build \\"x\\"" :why "[^"]*"\}\)$`)
	m := re.FindStringSubmatch(tree)
	if m == nil || m[1] != m[2] {
		t.Fatalf("tasks/tree %s = %s", build, tree)
	}
	// History puts the worker's ask under what began its job, boss's
	// jobs/run, so it is in the build's chain, not one of its own.
	es, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	var run, ask int64 = -1, -2
	for _, e := range es {
		switch {
		case e.Kind == "call" && e.From == "boss" && e.To == "jobs/run":
			run = e.ID
		case e.To == "helper/ask" && strings.HasPrefix(e.From, "agent/"):
			ask = e.Cause
		}
	}
	if ask != run {
		t.Errorf("the worker's ask is caused by %d, not boss's jobs/run %d", ask, run)
	}
	// The build waits on its work, not on boss to reply.
	if !strings.HasSuffix(tree, `:what ":boss/build \"x\"" :why "its work runs: 1 task open"})`) {
		t.Errorf("the build's why: %s", tree)
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

// TestTaskWatch watches a :stream job, as an agent's turn: each line it
// prints is published on :jobs/<id>, and tasks/watch reads claude's
// stream-json as steps; its result is the last line, and once it ends the
// watch says how.
func TestTaskWatch(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	script := `printf '%s\n' '{"type":"system","subtype":"init","model":"haiku"}' '{"type":"system","subtype":"compact_boundary"}' '{"type":"assistant","message":{"content":[{"type":"text","text":"Let me look."},{"type":"tool_use","name":"mcp__whiff__todo_list","input":{}}]}}' '{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"(ok [])"}]}]}}' 'plain text'
while [ ! -e go ]; do sleep 0.02; done
printf '%s\n' '{"subtype":"success","is_error":false,"result":"Nothing to do.","total_cost_usd":0.25,"num_turns":2,"type":"result"}'`
	r, _ := c.Message("call", "jobs/run", `[["sh" "-c" `+puff.Print(puff.String(script))+`] {:stream #t :as :text}]`, dir)
	v, _ := puff.ReadValue(r.Value)
	n, ok := pendingID(v)
	if !ok {
		t.Fatalf("jobs/run = %s", r.Value)
	}
	id := fmt.Sprint(n)
	watch := dial(t, dir)
	if _, err := watch.Sub("jobs/" + id); err != nil {
		t.Fatal(err)
	}
	view := func() string {
		t.Helper()
		resp, _ := c.Message("call", "tasks/watch", "["+id+"]", dir)
		return resp.Value
	}
	eventually(t, "the job's steps", func() string { return fmt.Sprint(strings.Contains(view(), "plain text")) }, "true")
	got := view()
	for _, want := range []string{
		`(ok (items [{:label "began  haiku"} {:label "compacted  the conversation: its older part summarized"} {:label "says  Let me look."} {:label "calls  todo_list {}"} {:label "answer  (ok [])"} {:label "plain text"}]`,
		`{:changed [:jobs/` + id + ` :tasks/changed]}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tasks/watch lacks %s:\n%s", want, got)
		}
	}
	// A line published after the watcher subscribed: the last, as the
	// others may have come before.
	os.WriteFile(filepath.Join(dir, "go"), nil, 0o600)
	for deadline := time.After(2 * time.Second); ; {
		select {
		case e := <-watch.Events():
			if e.Topic != "jobs/"+id || !strings.Contains(e.Value, "Nothing to do.") {
				continue
			}
		case <-deadline:
			t.Fatalf("no :jobs/%s event with the result", id)
		}
		break
	}
	res, _ := c.Message("wait", "", "["+id+"]", dir)
	if want := `(ok "{\"subtype\":\"success\"`; !strings.HasPrefix(res.Value, want) {
		t.Errorf("the result = %s, want only the last line", res.Value)
	}
	eventually(t, "the ended job's watch", func() string {
		return fmt.Sprint(strings.HasPrefix(view(), `(ok (items [{:label "ended ($0.25, 2 turns)  Nothing to do."} {:actions [{:action [:tasks/result `+id+`] :label "Result"}]`))
	}, "true")
	if r, _ := c.Message("call", "tasks/watch", "[99999]", dir); !strings.Contains(r.Value, ":no-id") {
		t.Errorf("tasks/watch of none = %s", r.Value)
	}
}
