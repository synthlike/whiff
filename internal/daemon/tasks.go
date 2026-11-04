package daemon

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// :tasks shows the tasks, what whiff answers pending and later with
// [:whiff/done id result]: a message the review gate holds, an ask
// waiting in an inbox, a job running, and each one that ended in the last
// day, with its result. So you see what was accepted, what ran, what
// failed and who owns the result, and act on it: cancel one that hasn't
// ended, or take an action its result offers, as pipeline's Retry. It
// shows; it decides nothing: what an action does is its service's.

// tasksShown is at most how many tasks that ended :tasks gives, the latest.
const tasksShown = 50

// task is a task as :tasks gives it.
type task struct {
	id       int64
	from     string    // its owner, whom its result goes to: "" for a client
	to       string    // where it is: the ask's or held message's address, jobs, or who answered it
	what     string    // what it does, cut short: :todo/add "x", or a job's command
	status   string    // held, waiting, running or ended
	at       time.Time // when it began, or for one that ended, when it ended
	reviewed bool      // you accepted it, or the chain it is in
	result   puff.Value
	held     *asked // one the review gate holds
	live     bool   // held: its sender is still here, so its session can be trusted
	// attempts is how many times a job ran, of tries, as :retry allows.
	attempts, tries int
	// stray is the process group a job that ended :unknown may still run
	// in, as when whiff died while it ran; 0 if none is there.
	stray int
}

// tasks is every task keep takes: those that haven't ended, oldest first,
// then those that ended in the last workKept, newest first, at most limit
// of them.
func (d *daemon) tasks(keep func(task) bool, limit int, now time.Time) []task {
	var open []task
	d.do(func(st *state) {
		for _, a := range st.asked {
			t := task{id: a.id, from: a.from, to: a.addr, what: a.label(), status: "waiting", at: a.at, reviewed: a.reviewed}
			if a.addr == "review" {
				t.to, t.status, t.held, t.live = a.to, "held", a, st.addrs[a.from] != nil
			}
			if keep(t) {
				open = append(open, t)
			}
		}
		for _, j := range st.jobs {
			at := now
			if len(j.attempts) > 0 {
				at = j.attempts[0].started
			}
			t := task{id: j.id, from: j.from, to: "jobs", what: cmdText(j.cmd), status: "running", at: at, reviewed: j.reviewed,
				attempts: len(j.attempts), tries: j.opts.retry + 1}
			if keep(t) {
				open = append(open, t)
			}
		}
	})
	slices.SortFunc(open, func(a, b task) int { return int(a.id - b.id) })
	// One that ends while this reads is found in both: the open one wins.
	isOpen := map[int64]bool{}
	for _, t := range open {
		isOpen[t.id] = true
	}
	var ended []task
	d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(tasksBucket).ForEach(func(k, v []byte) error {
			id := int64(binary.BigEndian.Uint64(k))
			if isOpen[id] {
				return nil
			}
			get, err := fields(string(v))
			if err != nil {
				return nil
			}
			from, _ := get("from").(puff.Keyword)
			at, ok := get("ended").(puff.Time)
			if !ok || now.Sub(at.Go()) > workKept {
				return nil
			}
			by, _ := get("by").(puff.Keyword)
			reviewed, _ := get("reviewed").(puff.Bool)
			t := task{id: id, from: string(from), to: string(by), status: "ended", at: at.Go(), reviewed: bool(reviewed), result: get("result")}
			if w, ok := get("what").(puff.String); ok {
				t.what = string(w)
			} else if cmd := get("cmd"); cmd != nil {
				t.what = cmdText(cmd)
			} else if by != "" {
				t.what = ":" + string(by)
			}
			if as, ok := get("attempts").(puff.Vector); ok {
				retry, _ := get("retry").(puff.Int)
				t.attempts, t.tries = as.Len(), int(retry)+1
			}
			if pid, _ := puff.Field(puff.ErrValue(t.result), "pid").(puff.Int); groupThere(int(pid)) {
				t.stray = int(pid)
			}
			if keep(t) {
				ended = append(ended, t)
			}
			return nil
		})
	})
	slices.SortFunc(ended, func(a, b task) int { return int(b.id - a.id) })
	if len(ended) > limit {
		ended = ended[:limit]
	}
	all := append(open, ended...)
	for i := range all {
		all[i].what = short(all[i].what)
	}
	return all
}

// argsText is args as a message's line shows them: " 12 \"x\"", or "".
func argsText(args puff.Vector) string {
	if args.Len() == 0 {
		return ""
	}
	return " " + strings.TrimSuffix(strings.TrimPrefix(puff.Print(args), "["), "]")
}

// cmdText is a job's command, [program args...], as it would be typed,
// quoting only the args that need it.
func cmdText(cmd puff.Value) string {
	vec, ok := cmd.(puff.Vector)
	if !ok {
		return puff.Print(cmd)
	}
	parts := make([]string, vec.Len())
	for i := range vec.Len() {
		if s, ok := vec.At(i).(puff.String); ok && s != "" && !strings.ContainsAny(string(s), " \t\n\"'") {
			parts[i] = string(s)
		} else {
			parts[i] = puff.Print(vec.At(i))
		}
	}
	return strings.Join(parts, " ")
}

// state is how t stands, a word: held, waiting or running; or for one that
// ended, done, failed, or how it failed: cancelled, rejected, interrupted.
// An (ok m) whose :status says it failed, gave up or was cancelled, as a
// pipeline build's, reads as that.
func (t task) state() string {
	if t.status != "ended" {
		return t.status
	}
	tag, ok := t.result.(puff.Tag)
	if !ok || tag.Len() != 1 {
		return "done"
	}
	if tag.Name() == "ok" {
		if s, _ := puff.Field(tag.At(0), "status").(puff.Keyword); s == "failed" || s == "gave-up" || s == "cancelled" {
			return string(s)
		}
		return "done"
	}
	switch k, _ := puff.Field(tag.At(0), "kind").(puff.Keyword); k {
	case "cancelled", "rejected", "interrupted":
		return string(k)
	}
	return "failed"
}

// failed says whether t ended without doing what it was for, and not
// because you cancelled or rejected it.
func (t task) failed() bool {
	switch t.state() {
	case "failed", "interrupted", "gave-up":
		return true
	}
	return false
}

// value is t as :tasks/list gives it.
func (t task) value() puff.Value {
	es := []puff.Entry{
		puff.Key("id", puff.Int(t.id)),
		puff.Key("status", puff.Keyword(t.status)),
		puff.Key("state", puff.Keyword(t.state())),
		puff.Key("what", puff.String(t.what)),
		puff.Key("at", puff.NewTime(t.at)),
	}
	if t.from != "" {
		es = append(es, puff.Key("from", puff.Keyword(t.from)))
	}
	if t.to != "" {
		es = append(es, puff.Key("to", puff.Keyword(t.to)))
	}
	if t.reviewed {
		es = append(es, puff.Key("reviewed", puff.Bool(true)))
	}
	if t.tries > 1 || t.attempts > 1 {
		es = append(es, puff.Key("attempts", puff.Int(t.attempts)), puff.Key("tries", puff.Int(t.tries)))
	}
	if t.stray != 0 {
		es = append(es, puff.Key("stray", puff.Int(t.stray)))
	}
	if t.result != nil {
		es = append(es, puff.Key("result", brief(t.result)))
	}
	m, _ := puff.NewMap(es...)
	return m
}

// item is t as an item of :tasks/view: its state and what it does, then
// its id, owner, who answered, when, its attempts, whether it may still
// run, and its result; and what can be done
// with it. One held has :review's actions; one waiting or running can be
// cancelled, and a job's output read; one that ended shows its result
// whole, and has the actions its result offers, as {:actions [...]} in its
// (ok m) or (err m).
func (t task) item(now time.Time) puff.Value {
	owner := "you"
	if t.from != "" {
		owner = ":" + t.from
	}
	parts := []string{fmt.Sprintf("task %d for %s", t.id, owner)}
	if t.status == "ended" && t.to != "" {
		parts = append(parts, "by :"+t.to)
	}
	parts = append(parts, ago(now.Sub(t.at)))
	switch {
	case t.status == "running" && t.tries > 1:
		parts = append(parts, fmt.Sprintf("attempt %d of %d", t.attempts, t.tries))
	case t.attempts > 1:
		parts = append(parts, fmt.Sprintf("%d attempts", t.attempts))
	}
	if t.stray != 0 {
		parts = append(parts, fmt.Sprintf("may still run: kill -- -%d stops it", t.stray))
	}
	// Once it ended, :reviewed may say only that you answered it.
	if t.reviewed && t.status != "ended" {
		parts = append(parts, "accepted")
	}
	if t.result != nil {
		parts = append(parts, puff.Print(brief(t.result)))
	}
	id := puff.Int(t.id)
	action := func(label, fn string) puff.Value {
		m, _ := puff.NewMap(puff.Key("label", puff.String(label)), puff.Key("action", puff.NewVector(puff.Keyword(fn), id)))
		return m
	}
	var actions []puff.Value
	add := func(v puff.Value) {
		if vec, ok := v.(puff.Vector); ok {
			for i := range vec.Len() {
				actions = append(actions, vec.At(i))
			}
		}
	}
	switch t.status {
	case "held":
		add(puff.Field(t.held.reviewItem(t.live), "actions"))
	case "waiting":
		actions = append(actions, action("Cancel", "tasks/cancel"))
	case "running":
		actions = append(actions, action("Output", "jobs/output"), action("Cancel", "tasks/cancel"))
	case "ended":
		actions = append(actions, action("Result", "tasks/result"))
		if tag, ok := t.result.(puff.Tag); ok && tag.Len() == 1 {
			if more := puff.Field(tag.At(0), "actions"); more != nil && isActions(more) == "" {
				add(more)
			}
		}
	}
	m, _ := puff.NewMap(
		puff.Key("label", puff.String(t.state()+"  "+t.what)),
		puff.Key("detail", puff.String(strings.Join(parts, " · "))),
		puff.Key("actions", puff.NewVector(actions...)),
	)
	return m
}

// ago is d, how long since, as a person says it: just now, 4m ago, 3h ago.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh ago", int(d/time.Hour))
}

// taskChanged publishes :tasks/changed, with task id, which began or ended:
// what a client showing :tasks/view fetches it again on.
func (st *state) taskChanged(id int64, user bool) {
	st.publish(message{from: "tasks", topic: "tasks/changed", args: puff.Print(puff.NewVector(puff.Int(id))), lineage: lineage{user: user}})
}

// tasksService is :tasks: the tasks as data and as a view, a task's whole
// result, and cancel, which is yours, as :review's functions are.
func (d *daemon) tasksService() *service {
	all := func(task) bool { return true }
	// view is the tasks keep takes as a view.
	view := func(keep func(task) bool) func(message, []puff.Value) (puff.Value, error) {
		return func(message, []puff.Value) (puff.Value, error) {
			now := time.Now()
			var items []puff.Value
			for _, t := range d.tasks(keep, tasksShown, now) {
				items = append(items, t.item(now))
			}
			opts, _ := puff.NewMap(puff.Key("changed", puff.Keyword("tasks/changed")))
			return puff.Ok(puff.NewTag("items", puff.NewVector(items...), opts)), nil
		}
	}
	return &service{name: "tasks", d: d, cur: caller{addr: "tasks"},
		fns: map[string]handler{
			"list": {mark: puff.MarkRead, marked: true,
				doc: fmt.Sprintf("(ok tasks): the tasks that haven't ended, oldest first, then the last %d that ended in the last day: each {:id :status :state :what :at :from :to :result}. :status is :held, :waiting, :running or :ended; :state says how it ended too: :done, :failed, :cancelled, :rejected, :interrupted, or its result's :status, as :gave-up. :from is its owner, who gets its result (none for you); :to, where it waits or who answered it; :reviewed #t, its chain passed the review gate; :attempts and :tries, how many times a job with :retry ran, of how many it may; :stray, the process group of a job that ended :unknown, while one is there: it may still be running.", tasksShown),
				native: func(message, []puff.Value) (puff.Value, error) {
					var vs []puff.Value
					for _, t := range d.tasks(all, tasksShown, time.Now()) {
						vs = append(vs, t.value())
					}
					return puff.Ok(puff.NewVector(vs...)), nil
				}},
			"view": {mark: puff.MarkRead, marked: true,
				doc:    "(ok view): the tasks as list gives them, as items: one held has :review's actions, one waiting or running Cancel, a job Output too, and one that ended Result, and the actions its result offers, as {:actions [...]} in its (ok m) or (err m).",
				native: view(all)},
			"failed": {mark: puff.MarkRead, marked: true,
				doc:    "(ok view): the tasks that failed in the last day, newest first, as view gives them: :failed, :interrupted, or a result's :status :failed or :gave-up; not those you cancelled or rejected.",
				native: view(task.failed)},
			"result": {params: []string{"id"}, mark: puff.MarkRead, marked: true, native: d.taskResult,
				doc: "(ok r), the result task id ended with, whole, while it is kept; (err {:kind :no-id}) if it hasn't ended, or isn't kept."},
			"cancel": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Cancel task id, as whiff cancel does: :done, or (err {:kind :ended :result r}) if it had ended.",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					id, ok := args[0].(puff.Int)
					if !ok {
						return nil, fmt.Errorf("want a task id, got %s", puff.Print(args[0]))
					}
					return d.cancel("", int64(id)), nil
				}},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

func (d *daemon) taskResult(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a task id, got %s", puff.Print(args[0]))
	}
	_, _, r, err := storedResult(d.db, int64(id))
	switch {
	case err != nil:
		return puff.Errorf("store", "%v", err), nil
	case r == nil:
		return puff.Errorf("no-id", "task %d hasn't ended, or its result isn't kept", id), nil
	}
	return puff.Ok(r), nil
}
