package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// tailSize bounds what a job keeps of each output stream: its end.
const tailSize = 64 << 10

// defaultJobTimeout is how long a job runs without a :timeout.
const defaultJobTimeout = time.Hour

// stopGrace is how long a stopped job's process group has to end after
// SIGTERM, before SIGKILL: time for a server to shut down cleanly and for
// docker run to stop its container. whiff stop waits it out, well within
// storeTimeout, which the next daemon waits for whiff.db.
var stopGrace = 3 * time.Second

// job is a process :jobs runs, an attempt at a time: it runs again, under
// the same id, only if it asked to and it's safe: see again.
type job struct {
	ticket
	cmd  puff.Vector
	opts jobOpts
	// ctx ends when the job is cancelled, or whiff stops, and each
	// attempt's with it. Only the goroutine running an attempt, or the
	// route goroutine, touches p, pid and attempts.
	ctx      context.Context
	cancel   context.CancelFunc
	p        *proc     // the last attempt, whose output :jobs/output reads
	pid      int       // the last attempt's process group, as it is the group's leader
	attempts []attempt // oldest first
}

// jobOpts is how a job runs, from :jobs/run's options.
type jobOpts struct {
	timeout    time.Duration // 0 for :timeout :none, a server that runs until stopped
	stdin      *string
	dir        string // where it runs
	text       bool   // :as :text
	retry      int    // how many times it may run again
	idempotent bool   // running it twice does what running it once does
}

// attempt is one try at a job. The last one ends with the job, which has
// its result; those before it have when and how they ended.
type attempt struct {
	started, ended time.Time
	outcome        string
	pid            int
}

// proc is an attempt's process, running.
type proc struct {
	cmd            *exec.Cmd
	ctx            context.Context // ends at the attempt's timeout, or with its job's
	cancel         context.CancelFunc
	stdout, stderr tail
	termed         atomic.Int64 // when its group got SIGTERM, in Unix nanoseconds; 0 until then
}

// jobsService is :jobs, whiff's own service for work longer than a call:
// its functions are Go, and its state, the running jobs, belongs to the
// route goroutine.
func (d *daemon) jobsService() *service {
	return &service{name: "jobs", d: d, cur: caller{addr: "jobs"},
		fns: map[string]handler{
			"run": {params: []string{"cmd", "opts"}, optional: 1, mark: puff.MarkWrite, marked: true, native: d.runJob, cmd: true,
				doc: "Run cmd, [program args...], here; pending now, {:exit :stdout :stderr} when done. Options: :timeout (1h), or :none for a server; :stdin, a string; :dir, where it runs; :as :text answers (ok stdout), or (err {:kind :exit}) unless it exits 0; :origin :agent, for a job whose output a model writes, answers as :agent's; :retry n runs it again, up to n times, under its id, if a timeout or whiff stopped it, or whiff died while it ran, and only if it is :read or :idempotent #t says running it twice is safe. :read for a read-only command, as whiff ls lists them."},
			"list": {mark: puff.MarkRead, marked: true, native: d.listJobs,
				doc: "The running jobs, each {:id :cmd :cwd :from}."},
			"cancel": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, native: d.cancelJob,
				doc: "Stop job id and everything it started: SIGTERM, then SIGKILL for what's left after 3s."},
			"output": {params: []string{"id"}, mark: puff.MarkRead, marked: true, native: d.jobOutput,
				doc: "(ok {:stdout :stderr}), what running job id has written so far: the last 64KiB of each. (err {:kind :no-id}) once it isn't running: its result is in whiff wait."},
			"process": {params: []string{"id"}, mark: puff.MarkRead, marked: true, native: d.jobProcess,
				doc: "(some pid), the process group job id may still run in: while it runs, and after whiff died while it ran, while its result's :pid is there; else (none). Retry its write only once it's (none)."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

func (d *daemon) runJob(m message, args []puff.Value) (puff.Value, error) {
	cmdv, opts, user, err := jobArgs(m, args)
	if err != nil {
		return nil, err
	}
	j := &job{cmd: cmdv, opts: opts}
	j.ctx, j.cancel = context.WithCancel(d.ctx)
	p, err := d.spawn(j)
	if err != nil {
		j.cancel()
		return puff.Errorf("start", "%v", err), nil
	}
	d.do(func(st *state) {
		// One you accepted is the task its sender already waits for.
		id := m.held
		if id == 0 {
			id = st.newID()
		}
		j.ticket = ticket{id: id, msg: m.id, from: m.from, dir: m.dir, user: user, session: m.session}
		j.began(p)
		st.jobs[j.id] = j
		if err := d.keepJob(j); err != nil {
			st.note(fmt.Sprintf("job %d isn't stored, so a crash won't tell its sender: %v", j.id, err))
		}
	})
	d.publish(message{cause: m.id, from: "jobs", topic: "jobs/started", args: puff.Print(puff.NewVector(j.value())), dir: m.dir, user: m.user})
	d.wg.Go(func() { d.await(j, p) })
	return pending(j.id, "job %d runs %s", j.id, j.argv()[0]), nil
}

// jobArgs reads :jobs/run's arguments, from m: the command, how it runs,
// and whether it answers as yours, which :origin :agent lowers.
func jobArgs(m message, args []puff.Value) (puff.Vector, jobOpts, bool, error) {
	o := jobOpts{timeout: defaultJobTimeout, dir: m.dir}
	user := m.user
	cmdv, ok := args[0].(puff.Vector)
	for i := range cmdv.Len() {
		_, isStr := cmdv.At(i).(puff.String)
		ok = ok && isStr
	}
	if !ok || cmdv.Len() == 0 {
		return cmdv, o, user, fmt.Errorf(`want a command like ["go" "test"], got %s`, puff.Print(args[0]))
	}
	if len(args) < 2 {
		return cmdv, o, user, nil
	}
	opts, ok := args[1].(puff.Map)
	if !ok {
		return cmdv, o, user, fmt.Errorf("want options as a map, got %s", puff.Print(args[1]))
	}
	for i := range opts.Len() {
		e := opts.Entry(i)
		t, isDur := e.Val.(puff.Duration)
		s, isStr := e.Val.(puff.String)
		n, isInt := e.Val.(puff.Int)
		b, isBool := e.Val.(puff.Bool)
		switch {
		case puff.Equal(e.Key, puff.Keyword("timeout")) && isDur && t > 0:
			o.timeout = time.Duration(t)
		case puff.Equal(e.Key, puff.Keyword("timeout")) && puff.Equal(e.Val, puff.Keyword("none")):
			o.timeout = 0
		case puff.Equal(e.Key, puff.Keyword("stdin")) && isStr:
			o.stdin = (*string)(&s)
		case puff.Equal(e.Key, puff.Keyword("dir")) && isStr:
			o.dir = string(s)
			if !filepath.IsAbs(o.dir) {
				o.dir = filepath.Join(m.dir, o.dir)
			}
		case puff.Equal(e.Key, puff.Keyword("origin")) && puff.Equal(e.Val, puff.Keyword("agent")):
			user = false
		case puff.Equal(e.Key, puff.Keyword("as")) && puff.Equal(e.Val, puff.Keyword("text")):
			o.text = true
		case puff.Equal(e.Key, puff.Keyword("retry")) && isInt && n > 0:
			o.retry = int(n)
		case puff.Equal(e.Key, puff.Keyword("idempotent")) && isBool:
			o.idempotent = bool(b)
		default:
			return cmdv, o, user, fmt.Errorf("want {:timeout d :stdin s :dir path :as :text :origin :agent :retry n :idempotent b}, a positive duration or :none, a count and strings, got %s", puff.Print(opts))
		}
	}
	return cmdv, o, user, nil
}

// argv is j's command as strings.
func (j *job) argv() []string {
	var argv []string
	for i := range j.cmd.Len() {
		s, _ := j.cmd.At(i).(puff.String)
		argv = append(argv, string(s))
	}
	return argv
}

// spawn starts an attempt at j, in its own process group, so a cancel or
// timeout stops what it started too: SIGTERM, then SIGKILL for what's left
// after stopGrace (see stop).
func (d *daemon) spawn(j *job) (*proc, error) {
	p := &proc{}
	if j.opts.timeout > 0 {
		p.ctx, p.cancel = context.WithTimeout(j.ctx, j.opts.timeout)
	} else {
		p.ctx, p.cancel = context.WithCancel(j.ctx)
	}
	argv := j.argv()
	cmd := exec.CommandContext(p.ctx, argv[0], argv[1:]...)
	cmd.Dir = j.opts.dir
	cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
	if j.opts.stdin != nil {
		cmd.Stdin = strings.NewReader(*j.opts.stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		p.termed.Store(time.Now().UnixNano())
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// Past it, Wait kills the leader, if it's still there, and stops reading
	// pipes a child it left still holds.
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		p.cancel()
		return nil, err
	}
	p.cmd = cmd
	return p, nil
}

// began records p as j's new attempt.
func (j *job) began(p *proc) {
	j.p, j.pid = p, p.cmd.Process.Pid
	j.attempts = append(j.attempts, attempt{started: time.Now(), pid: j.pid})
}

// ended records how j's last attempt ended, as another follows it.
func (j *job) ended(outcome string) {
	if n := len(j.attempts); n > 0 {
		j.attempts[n-1].ended, j.attempts[n-1].outcome = time.Now(), outcome
	}
}

// result waits for p, an attempt at j, and is how it ended. One that was
// stopped, by a timeout, a cancel or whiff stopping, keeps the end of
// what it printed, as one that ended on its own does.
func (d *daemon) result(j *job, p *proc) puff.Value {
	err := p.cmd.Wait()
	defer p.cancel()
	p.stop()
	var exitErr *exec.ExitError
	switch {
	case errors.Is(p.ctx.Err(), context.DeadlineExceeded):
		return p.withOutput(failed("timeout", "stopped", "job %d took longer than %v", j.id, j.opts.timeout))
	case p.ctx.Err() != nil && d.ctx.Err() != nil:
		return p.withOutput(failed("interrupted", "stopped", "job %d was killed when whiff stopped: what it had done stays done", j.id))
	case p.ctx.Err() != nil:
		return p.withOutput(failed("cancelled", "stopped", "job %d was cancelled", j.id))
	case err == nil, errors.As(err, &exitErr):
	default:
		return puff.Errorf("fault", "job %d: %v", j.id, err)
	}
	code := p.cmd.ProcessState.ExitCode()
	switch {
	case j.opts.text && code == 0:
		return puff.Ok(puff.String(p.stdout.String()))
	case j.opts.text:
		e := puff.Errorf("exit", "job %d exited %d", j.id, code).(puff.Tag).At(0).(puff.Map)
		e, _ = e.With(puff.Keyword("exit"), puff.Int(code))
		e, _ = e.With(puff.Keyword("stderr"), puff.String(p.stderr.String()))
		return puff.NewTag("err", e)
	}
	out, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("exit"), Val: puff.Int(code)},
		puff.Entry{Key: puff.Keyword("stdout"), Val: puff.String(p.stdout.String())},
		puff.Entry{Key: puff.Keyword("stderr"), Val: puff.String(p.stderr.String())},
	)
	return puff.Ok(out)
}

// withOutput is r, an (err e), with p's :stdout and :stderr in e.
func (p *proc) withOutput(r puff.Value) puff.Value {
	e := r.(puff.Tag).At(0).(puff.Map)
	e, _ = e.With(puff.Keyword("stdout"), puff.String(p.stdout.String()))
	e, _ = e.With(puff.Keyword("stderr"), puff.String(p.stderr.String()))
	return puff.NewTag("err", e)
}

// stop kills what's left of p's group stopGrace after its SIGTERM, if it
// got one: the leader may have ended and left children that ignore it. It
// waits for the group on the attempt's goroutine, so whiff stop waits too.
func (p *proc) stop() {
	t := p.termed.Load()
	if t == 0 {
		return
	}
	pid := p.cmd.Process.Pid
	for deadline := time.Unix(0, t).Add(stopGrace); groupThere(pid) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	// While the group is there, its id can't be another's.
	if groupThere(pid) {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// again says whether j runs again after an attempt that ended r: only if
// it asked to and has attempts left, a timeout or whiff stopped it rather
// than you, and running it twice is safe, as its command only reads or it
// says it is idempotent. Exiting non-zero isn't retried: a corrective
// loop, like a dev who failed the check, stays in a service.
func (j *job) again(r puff.Value) bool {
	kind := errKind(r)
	return (kind == "timeout" || kind == "interrupted") && len(j.attempts) <= j.opts.retry &&
		(j.opts.idempotent || puff.ReadOnlyCommand(j.cmd))
}

// errKind is the :kind of r, an (err e); "" for anything else.
func errKind(r puff.Value) string {
	k, _ := errField(r, "kind").(puff.Keyword)
	return string(k)
}

// errField is e's field key, for r an (err e); nil for anything else.
func errField(r puff.Value, key string) puff.Value {
	t, ok := r.(puff.Tag)
	if !ok || t.Name() != "err" || t.Len() != 1 {
		return nil
	}
	e, _ := t.At(0).(puff.Map)
	v, _ := e.Get(puff.Keyword(key))
	return v
}

// await waits for j's attempt p, and each one after it, then answers j's
// sender. One that whiff stops, and may run again, stays stored as it is
// for the next daemon to run again: see resume.
func (d *daemon) await(j *job, p *proc) {
	for {
		r := d.result(j, p)
		if !j.again(r) {
			d.end(j, r)
			return
		}
		var next *proc
		if d.ctx.Err() == nil {
			var err error
			if next, err = d.spawn(j); err != nil {
				d.note("job %d can't run again, so it ends as it did: %v", j.id, err)
				d.end(j, r)
				return
			}
		}
		step := func() {
			j.ended("stopped")
			if next != nil {
				j.began(next)
			}
			if err := d.keepJob(j); err != nil {
				fmt.Fprintf(d.log, "whiff: job %d's attempts aren't stored: %v\n", j.id, err)
			}
		}
		// Stopping, the route goroutine may have gone, and j with it.
		if !d.do(func(st *state) {
			step()
			if next != nil {
				st.note(fmt.Sprintf("job %d: %s; attempt %d of %d", j.id, errMessage(r), len(j.attempts), j.opts.retry+1))
			}
		}) {
			step()
		}
		if next == nil {
			return
		}
		p = next
	}
}

// errMessage is the :message of r, an (err e).
func errMessage(r puff.Value) string {
	e, _ := r.(puff.Tag).At(0).(puff.Map)
	m, _ := e.Get(puff.Keyword("message"))
	s, _ := m.(puff.String)
	return string(s)
}

// end answers j's sender r. Stopping, nobody can be told: the answer
// waits, unhandled, for the next daemon.
func (d *daemon) end(j *job, r puff.Value) {
	j.cancel()
	told := d.do(func(st *state) {
		delete(st.jobs, j.id)
		d.answer(st, j.ticket, "jobs", r)
	})
	if !told {
		if err := endTask(d.db, j.ticket, "jobs", r, false); err != nil {
			fmt.Fprintf(d.log, "whiff: job %d's answer isn't stored: %v\n", j.id, err)
		}
		return
	}
	d.publish(jobDone(j, r))
}

// rerun runs j, found stored at start, again under its id, as again allows
// after r: its last attempt ended as whiff stopped it, or whiff died while
// it ran and nothing of it still runs. If it can't start, its sender is
// told r.
func (d *daemon) rerun(j *job, r puff.Value) {
	j.ctx, j.cancel = context.WithCancel(d.ctx)
	if n := len(j.attempts); n > 0 && j.attempts[n-1].ended.IsZero() {
		j.ended("unknown")
	}
	p, err := d.spawn(j)
	d.do(func(st *state) {
		if err != nil {
			st.note(fmt.Sprintf("job %d can't run again, so it ends as it did: %v", j.id, err))
			d.answer(st, j.ticket, "jobs", r)
			st.publish(jobDone(j, r))
			return
		}
		j.began(p)
		st.jobs[j.id] = j
		if err := d.keepJob(j); err != nil {
			st.note(fmt.Sprintf("job %d's attempts aren't stored: %v", j.id, err))
		}
		st.note(fmt.Sprintf("job %d: %s; attempt %d of %d", j.id, errMessage(r), len(j.attempts), j.opts.retry+1))
	})
	if err == nil {
		d.wg.Go(func() { d.await(j, p) })
	}
}

// jobDone is the :jobs/done for j, answered r.
func jobDone(j *job, r puff.Value) message {
	done, _ := j.value().(puff.Map).With(puff.Keyword("result"), r)
	return message{cause: j.msg, from: "jobs", topic: "jobs/done", args: puff.Print(puff.NewVector(done)), dir: j.dir, user: j.user}
}

// lost is what the sender of j, found stored at start with no answer, is
// told: whiff stopped it, if its last attempt says so, else whiff died while
// it ran, so how it ended is unknown. It isn't killed if it still runs, as
// a pid can be reused: it may be someone else's now.
func lost(j *job) puff.Value {
	if n := len(j.attempts); n > 0 && !j.attempts[n-1].ended.IsZero() {
		return failed("interrupted", "stopped", "job %d was killed when whiff stopped: what it had done stays done", j.id)
	}
	msg := fmt.Sprintf("job %d was running when whiff died: how far it got is unknown", j.id)
	if !j.mayRun() {
		return failed("interrupted", "unknown", "%s", msg)
	}
	r := failed("interrupted", "unknown", "%s, and process group %d may still be running", msg, j.pid)
	e, _ := r.(puff.Tag).At(0).(puff.Map).With(puff.Keyword("pid"), puff.Int(j.pid))
	return puff.NewTag("err", e)
}

// mayRun says whether j's last process group may still run.
func (j *job) mayRun() bool { return groupThere(j.pid) }

// groupThere says whether process group pid is there.
func groupThere(pid int) bool {
	return pid > 1 && !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

func (d *daemon) jobProcess(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a job id, got %s", puff.Print(args[0]))
	}
	var pid int
	var err error
	d.do(func(st *state) {
		if j := st.jobs[int64(id)]; j != nil {
			pid = j.pid
			return
		}
		var r puff.Value
		// One whiff died while it ran has no result until resume, after
		// the services load: a :start asks before then.
		if i := slices.IndexFunc(st.lost, func(j *job) bool { return j.id == int64(id) }); i >= 0 {
			r = lost(st.lost[i])
		} else {
			_, _, r, err = storedResult(st.db, int64(id))
		}
		n, _ := errField(r, "pid").(puff.Int)
		pid = int(n)
	})
	switch {
	case err != nil:
		return puff.Errorf("store", "%v", err), nil
	case groupThere(pid):
		return puff.Ok(puff.Some(puff.Int(pid))), nil
	}
	return puff.Ok(puff.None()), nil
}

func (d *daemon) jobOutput(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a job id, got %s", puff.Print(args[0]))
	}
	var p *proc
	d.do(func(st *state) {
		if j := st.jobs[int64(id)]; j != nil {
			p = j.p
		}
	})
	if p == nil {
		return puff.Errorf("no-id", "no job %d is running: once it ends, whiff wait %d has its result", id, id), nil
	}
	out, _ := puff.NewMap(
		puff.Entry{Key: puff.Keyword("stdout"), Val: puff.String(p.stdout.String())},
		puff.Entry{Key: puff.Keyword("stderr"), Val: puff.String(p.stderr.String())},
	)
	return puff.Ok(out), nil
}

func (d *daemon) listJobs(message, []puff.Value) (puff.Value, error) {
	var jobs []puff.Value
	d.do(func(st *state) {
		for _, id := range slices.Sorted(maps.Keys(st.jobs)) {
			jobs = append(jobs, st.jobs[id].value())
		}
	})
	return puff.Ok(puff.NewVector(jobs...)), nil
}

func (d *daemon) cancelJob(_ message, args []puff.Value) (puff.Value, error) {
	id, ok := args[0].(puff.Int)
	if !ok {
		return nil, fmt.Errorf("want a job id, got %s", puff.Print(args[0]))
	}
	var j *job
	d.do(func(st *state) { j = st.jobs[int64(id)] })
	if j == nil {
		return puff.Errorf("no-id", "no job %d is running", id), nil
	}
	j.cancel()
	return puff.Done, nil
}

// value is j as :jobs/list shows it: {:id 3 :cmd ["go" "test"] :cwd "..."
// :from :gh}, without :from for a client.
func (j *job) value() puff.Value {
	es := []puff.Entry{
		{Key: puff.Keyword("id"), Val: puff.Int(j.id)},
		{Key: puff.Keyword("cmd"), Val: j.cmd},
		{Key: puff.Keyword("cwd"), Val: puff.String(j.dir)},
	}
	if j.from != "" {
		es = append(es, puff.Entry{Key: puff.Keyword("from"), Val: puff.Keyword(j.from)})
	}
	m, _ := puff.NewMap(es...)
	return m
}

// tail keeps the last tailSize bytes written to it. The process's
// goroutine writes it while :jobs/output reads it, so mu guards b.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailSize {
		t.b = append([]byte(nil), t.b[len(t.b)-tailSize:]...)
	}
	return len(p), nil
}

// String is the tail, without a character the cut split.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.b
	if len(b) > tailSize {
		b = b[len(b)-tailSize:]
	}
	return strings.ToValidUTF8(string(b), "")
}
