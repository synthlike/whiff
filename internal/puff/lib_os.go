package puff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // zones for time/to-string wherever puff runs
)

// path resolves p against the Env's directory.
func (c *Call) path(p string) string {
	if filepath.IsAbs(p) || c.Env.Dir == "" {
		return p
	}
	return filepath.Join(c.Env.Dir, p)
}

var osFns = []*Builtin{
	{Name: "io/println", Args: "v ...", Doc: "Print the values separated by spaces, strings as they are, then a newline.", Fn: func(c *Call, args []Value) (Value, error) {
		parts := make([]string, len(args))
		for i, v := range args {
			parts[i] = display(v)
		}
		fmt.Fprintln(c.Env.Stdout, strings.Join(parts, " "))
		return Done, nil
	}},
	{Name: "env/get", Args: "name", Doc: "(some s): the environment variable name; (none) if it isn't set.", Fn: str1(func(name string) Value {
		if s, ok := os.LookupEnv(name); ok {
			return Some(String(s))
		}
		return None()
	})},
	{Name: "os/cwd", Doc: "The working directory.", Fn: func(c *Call, _ []Value) (Value, error) {
		if c.Env.Dir != "" {
			return String(c.Env.Dir), nil
		}
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return String(dir), nil
	}},
}

var pathFns = []*Builtin{
	{Name: "path/join", Args: "part ...", Doc: "The parts joined with /, cleaned.", Fn: func(_ *Call, args []Value) (Value, error) {
		ss, err := strs(args)
		if err != nil {
			return nil, err
		}
		return String(filepath.Join(ss...)), nil
	}},
	{Name: "path/base", Args: "p", Doc: "The last element of p.", Fn: str1(func(p string) Value { return String(filepath.Base(p)) })},
	{Name: "path/dir", Args: "p", Doc: "p without its last element.", Fn: str1(func(p string) Value { return String(filepath.Dir(p)) })},
	{Name: "path/ext", Args: "p", Doc: "p's extension, with its dot: \".txt\"; \"\" if it has none.", Fn: str1(func(p string) Value { return String(filepath.Ext(p)) })},
	{Name: "path/abs", Args: "p", Doc: "p from the root, relative to the working directory.", Fn: func(c *Call, args []Value) (Value, error) {
		p, err := arg[String](args, 0, "a string")
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(c.path(string(p)))
		if err != nil {
			return nil, err
		}
		return String(abs), nil
	}},
}

// fileErr is an (err e) for a failed file operation: :not-found,
// :permission or :io.
func fileErr(err error) Value {
	kind := "io"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = "not-found"
	case errors.Is(err, fs.ErrPermission):
		kind = "permission"
	}
	return Errorf(kind, "%s", err)
}

// onPath makes a builtin of a function of a path, resolved, and the rest.
func onPath(f func(path string, args []Value) (Value, error)) builtinFn {
	return func(c *Call, args []Value) (Value, error) {
		p, err := arg[String](args, 0, "a path string")
		if err != nil {
			return nil, err
		}
		return f(c.path(string(p)), args)
	}
}

var fileFns = []*Builtin{
	{Name: "file/read-text", Args: "path [opts]", Doc: "(ok s): the file's contents.", Fn: onPath(func(path string, args []Value) (Value, error) {
		if _, err := options(args, 1); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fileErr(err), nil
		}
		return Ok(String(b)), nil
	})},
	{Name: "file/write", Args: "path s", Mark: MarkWrite, Doc: "Write s to the file, replacing what it held.", Fn: onPath(func(path string, args []Value) (Value, error) {
		s, err := arg[String](args, 1, "a string")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			return fileErr(err), nil
		}
		return Done, nil
	})},
	{Name: "file/write-atomic", Args: "path s", Mark: MarkWrite, Doc: "Write s to the file so a reader sees either the old contents or all of s.", Fn: onPath(func(path string, args []Value) (Value, error) {
		s, err := arg[String](args, 1, "a string")
		if err != nil {
			return nil, err
		}
		if err := writeAtomic(path, []byte(s)); err != nil {
			return fileErr(err), nil
		}
		return Done, nil
	})},
	{Name: "file/append", Args: "path s", Mark: MarkWrite, Doc: "Add s to the end of the file, creating it if there isn't one.", Fn: onPath(func(path string, args []Value) (Value, error) {
		s, err := arg[String](args, 1, "a string")
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fileErr(err), nil
		}
		_, err = f.WriteString(string(s))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fileErr(err), nil
		}
		return Done, nil
	})},
	{Name: "file/mkdir", Args: "path", Mark: MarkWrite, Doc: "Make the directory at path, and any parents it needs; :done if it is already there.", Fn: onPath(func(path string, _ []Value) (Value, error) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fileErr(err), nil
		}
		return Done, nil
	})},
	{Name: "file/exists?", Args: "path", Doc: "Whether there is a file or directory at path.", Fn: onPath(func(path string, _ []Value) (Value, error) {
		_, err := os.Stat(path)
		return Bool(err == nil), nil
	})},
	{Name: "file/list", Args: "dir", Doc: "(ok names): the names in dir, sorted.", Fn: onPath(func(path string, _ []Value) (Value, error) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fileErr(err), nil
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return Ok(strVec(names)), nil
	})},
}

// writeAtomic writes data to a temporary file next to path and renames it
// over path.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly after the rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// maxOutput bounds what proc/run keeps of each stream.
const maxOutput = 16 << 20

// capped is a buffer that refuses to grow past maxOutput.
type capped struct {
	b    bytes.Buffer
	over bool
}

func (w *capped) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > maxOutput {
		w.over = true
		return 0, errors.New("output too big")
	}
	return w.b.Write(p)
}

var procFns = []*Builtin{
	{Name: "proc/run", Args: "cmd [opts]", Mark: MarkWrite, Doc: "Run cmd, a vector of the program and its arguments, without a shell: (ok {:exit :stdout :stderr}), whatever the exit code. Options: :stdin, a string; :dir; :timeout, a duration.", Fn: procRun},
	{Name: "proc/output", Args: "cmd [opts]", Mark: MarkWrite, Doc: "Run cmd as proc/run does: (ok stdout) if it exits 0, else (err {:kind :exit :exit :stdout :stderr}) with its stderr in :message.", Fn: procOutput},
}

func procOutput(c *Call, args []Value) (Value, error) {
	r, err := procRun(c, args)
	if err != nil {
		return nil, err
	}
	t := r.(Tag)
	if t.Name() != "ok" {
		return r, nil
	}
	out := t.At(0).(Map)
	code, _ := out.Get(Keyword("exit"))
	stdout, _ := out.Get(Keyword("stdout"))
	stderr, _ := out.Get(Keyword("stderr"))
	if code == Int(0) {
		return Ok(stdout), nil
	}
	name := Print(args[0].(Vector).At(0))
	if s, ok := args[0].(Vector).At(0).(String); ok {
		name = string(s)
	}
	msg := fmt.Sprintf("%s exited %d", name, code)
	if e := strings.TrimSpace(string(stderr.(String))); e != "" {
		msg += ": " + e
	}
	e, _ := NewMap(
		Entry{Keyword("kind"), Keyword("exit")},
		Entry{Keyword("message"), String(msg)},
		Entry{Keyword("exit"), code},
		Entry{Keyword("stdout"), stdout},
		Entry{Keyword("stderr"), stderr},
	)
	return NewTag("err", e), nil
}

func procRun(c *Call, args []Value) (Value, error) {
	v, err := arg[Vector](args, 0, "a vector")
	if err != nil {
		return nil, err
	}
	argv, err := strs(v.items)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("want a command and its arguments, got []")
	}
	opts, err := options(args, 1, "stdin", "dir", "timeout")
	if err != nil {
		return nil, err
	}
	ctx := c.Ctx
	var timeout Duration
	if t, ok := opts.Get(Keyword("timeout")); ok {
		if timeout, ok = t.(Duration); !ok || timeout <= 0 {
			return nil, fmt.Errorf("want a positive :timeout duration, got %s", Print(t))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout))
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = c.Env.Dir
	if d, ok := opts.Get(Keyword("dir")); ok {
		s, ok := d.(String)
		if !ok {
			return nil, fmt.Errorf("want a :dir string, got %s", Print(d))
		}
		cmd.Dir = c.path(string(s))
	}
	if in, ok := opts.Get(Keyword("stdin")); ok {
		s, ok := in.(String)
		if !ok {
			return nil, fmt.Errorf("want a :stdin string, got %s", Print(in))
		}
		cmd.Stdin = strings.NewReader(string(s))
	}
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Its own process group, so a timeout or cancel kills what it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case c.Ctx.Err() != nil:
		return nil, errors.New("cancelled")
	case ctx.Err() != nil:
		return Errorf("timeout", "%s took longer than %s", argv[0], Print(timeout)), nil
	case stdout.over || stderr.over:
		return Errorf("too-big", "%s wrote more than %d bytes", argv[0], maxOutput), nil
	case err == nil, errors.As(err, &exitErr):
	case errors.Is(err, exec.ErrNotFound):
		return Errorf("not-found", "%s", err), nil
	default:
		return Errorf("io", "%s", err), nil
	}
	m, _ := NewMap(
		Entry{Keyword("exit"), Int(cmd.ProcessState.ExitCode())},
		Entry{Keyword("stdout"), String(stdout.b.String())},
		Entry{Keyword("stderr"), String(stderr.b.String())},
	)
	return Ok(m), nil
}

// Arithmetic and comparison on times are +, - and <, as for durations.
var timeFns = []*Builtin{
	{Name: "time/now", Doc: "The time now: 2026-10-27T18:25:00.123456Z.", Fn: func(*Call, []Value) (Value, error) {
		return NewTime(time.Now()), nil
	}},
	{Name: "time/from-string", Args: "s", Doc: "(ok t): s read as an RFC 3339 time, with any offset.", Fn: str1(func(s string) Value {
		t, err := time.Parse(time.RFC3339Nano, s)
		switch {
		case err != nil:
			return Errorf("parse", "not an RFC 3339 time: %q", s)
		case !timeFits(t):
			return Errorf("parse", "%s is out of range: times are from 1678 to 2262", s)
		}
		return Ok(NewTime(t))
	})},
	{Name: "time/to-string", Args: "t [opts]", Doc: "t in RFC 3339, in UTC or in {:zone \"Europe/Warsaw\"}.", Fn: func(_ *Call, args []Value) (Value, error) {
		t, err := arg[Time](args, 0, "a time")
		if err != nil {
			return nil, err
		}
		opts, err := options(args, 1, "zone")
		if err != nil {
			return nil, err
		}
		loc := time.UTC
		if z, ok := opts.Get(Keyword("zone")); ok {
			name, ok := z.(String)
			if !ok {
				return nil, fmt.Errorf("want a :zone string, got %s", Print(z))
			}
			if loc, err = time.LoadLocation(string(name)); err != nil {
				return nil, fmt.Errorf("unknown time zone %s", name)
			}
		}
		return String(t.Go().In(loc).Format("2006-01-02T15:04:05.999999999Z07:00")), nil
	}},
	{Name: "time/from-unix-ms", Args: "ms", Doc: "The time ms milliseconds after 1970-01-01 UTC.", Fn: func(_ *Call, args []Value) (Value, error) {
		ms, err := arg[Int](args, 0, "an int")
		if err != nil {
			return nil, err
		}
		t := time.UnixMilli(int64(ms))
		if !timeFits(t) {
			return nil, fmt.Errorf("%d ms is out of range: times are from 1678 to 2262", ms)
		}
		return NewTime(t), nil
	}},
	{Name: "time/to-unix-ms", Args: "t", Doc: "Milliseconds from 1970-01-01 UTC to t.", Fn: func(_ *Call, args []Value) (Value, error) {
		t, err := arg[Time](args, 0, "a time")
		if err != nil {
			return nil, err
		}
		return Int(t.Go().UnixMilli()), nil
	}},
	{Name: "time/since", Args: "t", Doc: "The duration from t to now.", Fn: func(_ *Call, args []Value) (Value, error) {
		t, err := arg[Time](args, 0, "a time")
		if err != nil {
			return nil, err
		}
		return Duration(time.Since(t.Go())), nil
	}},
}
