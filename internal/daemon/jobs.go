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
	"syscall"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// tailSize bounds what a job keeps of each output stream: its end.
const tailSize = 64 << 10

// defaultJobTimeout is how long a job runs without a :timeout.
const defaultJobTimeout = time.Hour

// job is a process :jobs runs.
type job struct {
	ticket
	cmd    puff.Vector
	cancel context.CancelFunc
	pid    int // its process group, as it is the group's leader
}

// jobsService is :jobs, whiff's own service for work longer than a call:
// its functions are Go, and its state, the running jobs, belongs to the
// route goroutine.
func (d *daemon) jobsService() *service {
	return &service{name: "jobs", d: d, cur: caller{addr: "jobs"},
		fns: map[string]handler{
			"run": {params: []string{"cmd", "opts"}, optional: 1, mark: puff.MarkWrite, marked: true, native: d.runJob, cmd: true,
				doc: "Run cmd, [program args...], here; pending now, {:exit :stdout :stderr} when done. Options: :timeout (1h); :stdin, a string; :dir, where it runs; :as :text answers (ok stdout), or (err {:kind :exit}) unless it exits 0; :origin :agent, for a job whose output a model writes, answers as :agent's. :read for a read-only command, as whiff ls lists them."},
			"list": {mark: puff.MarkRead, marked: true, native: d.listJobs,
				doc: "The running jobs, each {:id :cmd :cwd :from}."},
			"cancel": {params: []string{"id"}, mark: puff.MarkWrite, marked: true, native: d.cancelJob,
				doc: "Kill job id and everything it started."},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

func (d *daemon) runJob(m message, args []puff.Value) (puff.Value, error) {
	cmdv, ok := args[0].(puff.Vector)
	var argv []string
	for i := range cmdv.Len() {
		s, isStr := cmdv.At(i).(puff.String)
		ok = ok && isStr
		argv = append(argv, string(s))
	}
	if !ok || len(argv) == 0 {
		return nil, fmt.Errorf(`want a command like ["go" "test"], got %s`, puff.Print(args[0]))
	}
	timeout := defaultJobTimeout
	var stdin *string
	text := false // :as :text
	dir := m.dir
	user := m.user // :origin :agent lowers it
	if len(args) > 1 {
		opts, ok := args[1].(puff.Map)
		if !ok {
			return nil, fmt.Errorf("want options as a map, got %s", puff.Print(args[1]))
		}
		for i := range opts.Len() {
			e := opts.Entry(i)
			t, isDur := e.Val.(puff.Duration)
			s, isStr := e.Val.(puff.String)
			switch {
			case puff.Equal(e.Key, puff.Keyword("timeout")) && isDur && t > 0:
				timeout = time.Duration(t)
			case puff.Equal(e.Key, puff.Keyword("stdin")) && isStr:
				stdin = (*string)(&s)
			case puff.Equal(e.Key, puff.Keyword("dir")) && isStr:
				dir = string(s)
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(m.dir, dir)
				}
			case puff.Equal(e.Key, puff.Keyword("origin")) && puff.Equal(e.Val, puff.Keyword("agent")):
				user = false
			case puff.Equal(e.Key, puff.Keyword("as")) && puff.Equal(e.Val, puff.Keyword("text")):
				text = true
			default:
				return nil, fmt.Errorf("want {:timeout d :stdin s :dir path :as :text :origin :agent}, a positive duration and strings, got %s", puff.Print(opts))
			}
		}
	}

	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr tail
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = strings.NewReader(*stdin)
	}
	// Its own process group, so a cancel or timeout kills what it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return puff.Errorf("start", "%v", err), nil
	}

	j := &job{cmd: cmdv, cancel: cancel, pid: cmd.Process.Pid}
	d.do(func(st *state) {
		// One you accepted is the task its sender already waits for.
		id := m.held
		if id == 0 {
			id = st.newID()
		}
		j.ticket = ticket{id: id, msg: m.id, from: m.from, dir: m.dir, user: user, session: m.session}
		st.jobs[j.id] = j
		if err := d.keepJob(j); err != nil {
			st.note(fmt.Sprintf("job %d isn't stored, so a crash won't tell its sender: %v", j.id, err))
		}
	})
	d.publish(message{cause: m.id, from: "jobs", topic: "jobs/started", args: puff.Print(puff.NewVector(j.value())), dir: m.dir, user: m.user})
	d.wg.Go(func() {
		err := cmd.Wait()
		var r puff.Value
		var exitErr *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			r = failed("timeout", "stopped", "job %d took longer than %v", j.id, timeout)
		case ctx.Err() != nil && d.ctx.Err() != nil:
			r = failed("interrupted", "stopped", "job %d was killed when whiff stopped: what it had done stays done", j.id)
		case ctx.Err() != nil:
			r = failed("cancelled", "stopped", "job %d was cancelled", j.id)
		case err == nil, errors.As(err, &exitErr):
			out, _ := puff.NewMap(
				puff.Entry{Key: puff.Keyword("exit"), Val: puff.Int(cmd.ProcessState.ExitCode())},
				puff.Entry{Key: puff.Keyword("stdout"), Val: puff.String(stdout.String())},
				puff.Entry{Key: puff.Keyword("stderr"), Val: puff.String(stderr.String())},
			)
			r = puff.Ok(out)
			if code := cmd.ProcessState.ExitCode(); text && code == 0 {
				r = puff.Ok(puff.String(stdout.String()))
			} else if text {
				e := puff.Errorf("exit", "job %d exited %d", j.id, code).(puff.Tag).At(0).(puff.Map)
				e, _ = e.With(puff.Keyword("exit"), puff.Int(code))
				e, _ = e.With(puff.Keyword("stderr"), puff.String(stderr.String()))
				r = puff.NewTag("err", e)
			}
		default:
			r = puff.Errorf("fault", "job %d: %v", j.id, err)
		}
		cancel()
		// Stopping, nobody can be told: the answer waits, unhandled, for the
		// next daemon.
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
	})
	return pending(j.id, "job %d runs %s", j.id, argv[0]), nil
}

// jobDone is the :jobs/done for j, answered r.
func jobDone(j *job, r puff.Value) message {
	done, _ := j.value().(puff.Map).With(puff.Keyword("result"), r)
	return message{cause: j.msg, from: "jobs", topic: "jobs/done", args: puff.Print(puff.NewVector(done)), dir: j.dir, user: j.user}
}

// lost is what the sender of j, found stored at start with no answer, is
// told: whiff died while it ran, so how it ended is unknown. It isn't killed
// if it still runs, as a pid can be reused: it may be someone else's now.
func lost(j *job) puff.Value {
	msg := fmt.Sprintf("job %d was running when whiff died: how far it got is unknown", j.id)
	if j.pid <= 1 || errors.Is(syscall.Kill(-j.pid, 0), syscall.ESRCH) {
		return failed("interrupted", "unknown", "%s", msg)
	}
	r := failed("interrupted", "unknown", "%s, and process group %d may still be running", msg, j.pid)
	e, _ := r.(puff.Tag).At(0).(puff.Map).With(puff.Keyword("pid"), puff.Int(j.pid))
	return puff.NewTag("err", e)
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

// tail keeps the last tailSize bytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailSize {
		t.b = append([]byte(nil), t.b[len(t.b)-tailSize:]...)
	}
	return len(p), nil
}

// String is the tail, without a character the cut split.
func (t *tail) String() string {
	b := t.b
	if len(b) > tailSize {
		b = b[len(b)-tailSize:]
	}
	return strings.ToValidUTF8(string(b), "")
}
