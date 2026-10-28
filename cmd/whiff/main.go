// Command whiff is the whiff daemon and its clients.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/synthlike/whiff/internal/daemon"
	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

const usage = `usage:
  whiff call address/fn args...   call a function and print its value
  whiff send address/fn args...   send a message without waiting
  whiff pub topic value           publish value to the topic's subscribers
  whiff sub [topic...]            print what is published to the topics, every topic by default, until stopped
  whiff try file.puff fn args...  load a service file, call its function once and unload it
  whiff inbox [address]           list the messages waiting for a reply: yours, :me's, by default
  whiff reply id value            answer a message waiting for a reply
  whiff wait id                   print the answer to a pending message or job, waiting for it
  whiff pending                   list agents' messages that wait for your review
  whiff accept id                 deliver a message that waits for review, and print its value
  whiff reject id reason          answer a message that waits for review with (err {:kind :rejected})
  whiff trust agent/n service...  let an agent session's messages to the services through, as if you accepted them
  whiff free agent/n              let everything an agent session sends, publishes and evaluates through
  whiff gate agent/n              hold an agent session's :write messages for review again, as by default
  whiff session agent/n           what an agent session wrote, and how: held, accepted, trusted or free
  whiff eval code                 evaluate puff code in the daemon and print its value
  whiff repl                      evaluate forms as they're typed, in the daemon, keeping what they define
  whiff history                   print every message, newest first
  whiff history -f                print the last 10, oldest first, then each new one until stopped
  whiff ls                        list every address's functions and their marks, each agent session's mode, then the :read overrides
  whiff state service             print a service's state, as it last committed it
  whiff reset service             forget a service's stored state, so it starts from its :state again
  whiff mcp                       serve whiff to an agent over MCP on stdin and stdout, as :agent/<n>
  whiff stop                      stop the daemon
  whiff daemon                    run the daemon in the foreground

trust and free take --for 2h to end the mode after that long; else it lasts
until the session ends.

Each arg of call, send, pub, try and reply is read as a puff value, or taken as
a string if it doesn't read as one: 12 is 12, ann is "ann", '"12"' is "12".
A reply that isn't (ok v) or (err e) is sent as (ok value).
`

// stdin is what whiff repl reads; a var so tests can type into it.
var stdin io.Reader = os.Stdin

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "whiff: %v\n", err)
		os.Exit(1)
	}
	services := filepath.Join(home, ".whiff", "services")
	os.Exit(run(os.Args[1:], rpc.DefaultDir(), services, os.Stdout, os.Stderr))
}

// run is whiff with its arguments, the socket and services directories and
// its streams, returning the exit code.
func run(args []string, dir, services string, stdout, stderr io.Writer) int {
	switch {
	case len(args) >= 2 && (args[0] == "call" || args[0] == "send"):
		return message(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) >= 1 && args[0] == "sub":
		return sub(dir, args[1:], stdout, stderr)
	case len(args) >= 3 && args[0] == "try":
		return try(dir, args[1], args[2], args[3:], stdout, stderr)
	case len(args) == 3 && args[0] == "pub":
		return message(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) == 3 && args[0] == "reply", len(args) == 2 && args[0] == "wait",
		len(args) == 2 && args[0] == "accept", len(args) == 3 && args[0] == "reject":
		return message(dir, args[0], "", args[1:], stdout, stderr)
	case len(args) >= 2 && (args[0] == "trust" && len(args) >= 3 || args[0] == "free" || args[0] == "gate"):
		return setMode(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) == 2 && args[0] == "session":
		session := func(c *rpc.Client) (string, error) {
			es, err := c.History()
			return rpc.SessionText(es, args[1]), err
		}
		return text(dir, session, stdout, stderr)
	case len(args) == 1 && args[0] == "pending":
		pending := func(c *rpc.Client) (string, error) { return c.Inbox("review") }
		return text(dir, pending, stdout, stderr)
	case len(args) <= 2 && len(args) >= 1 && args[0] == "inbox":
		addr := "me"
		if len(args) == 2 {
			addr = args[1]
		}
		inbox := func(c *rpc.Client) (string, error) { return c.Inbox(addr) }
		return text(dir, inbox, stdout, stderr)
	case len(args) == 1 && args[0] == "repl":
		return repl(dir, stdin, stdout, stderr)
	case len(args) == 2 && args[0] == "eval":
		return eval(dir, args[1], stdout, stderr)
	case len(args) == 1 && args[0] == "history":
		return history(dir, false, stdout, stderr)
	case len(args) == 2 && args[0] == "history" && args[1] == "-f":
		return history(dir, true, stdout, stderr)
	case len(args) == 1 && args[0] == "ls":
		return text(dir, (*rpc.Client).Ls, stdout, stderr)
	case len(args) == 2 && (args[0] == "reset" || args[0] == "state"):
		return serviceOp(dir, args[0], args[1], stdout, stderr)
	case len(args) == 1 && args[0] == "mcp":
		return mcpRun(dir, stderr)
	case len(args) == 1 && args[0] == "stop":
		return stop(dir, stderr)
	case len(args) == 1 && args[0] == "daemon":
		return runDaemon(dir, services, stderr)
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help"):
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// connect connects to the daemon, starting it if it isn't running, and
// checks it speaks this whiff's protocol. On failure it reports why and
// returns nil.
func connect(dir string, stderr io.Writer) *rpc.Client {
	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return nil
	}
	if err := c.Compatible(); err != nil {
		c.Close()
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return nil
	}
	return c
}

// message calls, sends, publishes, replies, waits, accepts or rejects (op).
// A call, wait or accept prints the value of (ok v), a string as it is; (err
// e) prints e and exits 1. The others print nothing. A reject's reason is
// a string, whatever it reads as.
func message(dir, op, target string, args []string, stdout, stderr io.Writer) int {
	vals := make([]puff.Value, len(args))
	for i, a := range args {
		vals[i] = shellArg(a)
	}
	if op == "reject" {
		vals[1] = puff.String(args[1])
	}
	if op == "reply" {
		if r, ok := vals[1].(puff.Tag); !ok || r.Len() != 1 || r.Name() != "ok" && r.Name() != "err" {
			vals[1] = puff.Ok(vals[1])
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Message(op, target, puff.Print(puff.NewVector(vals...)), wd)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return result(op, resp, stdout, stderr)
}

// try loads file under a scratch address, calls its function fn with args
// and prints the reply as call does. A file that doesn't load prints its
// error, at file:line:col, and exits 1.
func try(dir, file, fn string, args []string, stdout, stderr io.Writer) int {
	src, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	vals := make([]puff.Value, len(args))
	for i, a := range args {
		vals[i] = shellArg(a)
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Try(file, string(src), fn, puff.Print(puff.NewVector(vals...)), wd)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	}
	return result("call", resp, stdout, stderr)
}

// result prints resp's Result for op: the value of (ok v) for a call, a
// wait or an accept, a string as it is; (err e) prints e and exits 1, except a pending
// call, which prints how to wait for its answer.
func result(op string, resp rpc.Response, stdout, stderr io.Writer) int {
	v, err := puff.ReadValue(resp.Value)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 {
		fmt.Fprintf(stderr, "whiff: want a Result, got %s\n", resp.Value)
		return 1
	}
	e, _ := r.At(0).(puff.Map)
	kind, _ := e.Get(puff.Keyword("kind"))
	id, _ := e.Get(puff.Keyword("id"))
	switch {
	case r.Name() == "err" && op == "call" && puff.Equal(kind, puff.Keyword("pending")) && id != nil:
		fmt.Fprintf(stdout, "pending %s (whiff wait %[1]s)\n", puff.Print(id))
	case r.Name() == "err":
		fmt.Fprintf(stderr, "whiff: %s\n", puff.Print(r.At(0)))
		return 1
	case op == "call" || op == "wait" || op == "accept" || op == "state":
		if s, ok := r.At(0).(puff.String); ok {
			fmt.Fprintln(stdout, string(s))
		} else {
			fmt.Fprintln(stdout, puff.Print(r.At(0)))
		}
	}
	return 0
}

// setMode sets agent's mode: trust (with services), free or gate, which
// takes no args. --for 2h, before or among the services, ends it after
// that long.
func setMode(dir, op, agent string, args []string, stdout, stderr io.Writer) int {
	var services []string
	var d time.Duration
	for i := 0; i < len(args); i++ {
		if args[i] != "--for" {
			services = append(services, args[i])
			continue
		}
		var err error
		if i+1 < len(args) {
			d, err = time.ParseDuration(args[i+1])
		}
		if i+1 == len(args) || err != nil || d <= 0 || op == "gate" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		i++
	}
	if op == "gate" && len(services) > 0 || op == "free" && len(services) > 0 || op == "trust" && len(services) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	mode := map[string]string{"trust": "trusted", "free": "free", "gate": "gated"}[op]
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Mode(strings.TrimPrefix(agent, ":"), mode, services, d)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return result(op, resp, stdout, stderr)
}

// serviceOp evaluates (op :service), reset or state: reset has the daemon
// forget service's stored state and reload it, and prints nothing; state
// prints its state. (err e) prints e and exits 1.
func serviceOp(dir, op, service string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval("("+op+" "+puff.Print(puff.Keyword(strings.TrimPrefix(service, ":")))+")", "")
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	}
	return result(op, resp, stdout, stderr)
}

// sub prints each value published to topics, every topic if there are
// none, a line each, until the daemon stops:
//
//	09:00:40.788  :jobs/done  user   :jobs  {:id 3 ...}
func sub(dir string, topics []string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	if _, err := c.Sub(topics...); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	for e := range c.Events() {
		from := "client"
		if e.From != "" {
			from = ":" + e.From
		}
		fmt.Fprintf(stdout, "%s  :%s  %-5s  %s  %s\n", e.At.Local().Format("15:04:05.000"), e.Topic, e.Origin, from, e.Value)
	}
	return 0
}

// history prints history newest first; or, to follow it, the last 10
// entries oldest first and then each new one, until the daemon stops.
func history(dir string, follow bool, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	if !follow {
		es, err := c.History()
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		io.WriteString(stdout, rpc.HistoryText(es))
		return 0
	}
	es, err := c.Trace()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	for _, e := range es[max(len(es)-10, 0):] {
		fmt.Fprintln(stdout, e)
	}
	for e := range c.Entries() {
		fmt.Fprintln(stdout, e)
	}
	return 0
}

// shellArg reads a command-line argument as a puff value, or takes it as a
// string if it doesn't read as one.
func shellArg(s string) puff.Value {
	if v, err := puff.ReadValue(s); err == nil {
		return v
	}
	return puff.String(s)
}

// eval prints src's value as the daemon evaluates it: exit 0, 1 for an
// error, 70 for a fault, like puff -e.
func eval(dir, src string, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval(src, wd)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	io.WriteString(stdout, resp.Out)
	switch {
	case resp.Fault:
		fmt.Fprintln(stderr, resp.Error)
		return 70
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	case resp.Value != "":
		fmt.Fprintln(stdout, resp.Value)
	}
	return 0
}

// repl reads puff from in a line at a time and evaluates it in the daemon,
// in one env for the session, printing each value; it keeps reading while a
// form or string is still open. An interrupt cancels the evaluation, or ends
// the REPL at its prompt.
func repl(dir string, in io.Reader, stdout, stderr io.Writer) int {
	interactive := false
	if f, ok := in.(*os.File); ok {
		fi, err := f.Stat()
		interactive = err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()

	var running atomic.Bool
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		for range interrupts {
			if running.Load() {
				c.Cancel()
				continue
			}
			fmt.Fprintln(stdout)
			os.Exit(130)
		}
	}()
	// evalInput reports whether the daemon is still there.
	evalInput := func(src string) bool {
		running.Store(true)
		resp, err := c.Repl(src, wd)
		running.Store(false)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return false
		}
		io.WriteString(stdout, resp.Out)
		switch {
		case resp.Error != "":
			fmt.Fprintln(stderr, resp.Error)
		case resp.Value != "":
			fmt.Fprintln(stdout, resp.Value)
		}
		return true
	}

	sc := bufio.NewScanner(in)
	var buf strings.Builder
	for {
		if interactive {
			if buf.Len() == 0 {
				fmt.Fprint(stdout, "whiff> ")
			} else {
				fmt.Fprint(stdout, "   ... ")
			}
		}
		if !sc.Scan() {
			break
		}
		buf.WriteString(sc.Text())
		buf.WriteByte('\n')
		var perr *puff.Error
		if _, err := puff.Read("", buf.String()); errors.As(err, &perr) && perr.Unclosed {
			continue
		}
		if !evalInput(buf.String()) {
			return 1
		}
		buf.Reset()
	}
	if interactive {
		fmt.Fprintln(stdout)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	if buf.Len() > 0 && !evalInput(buf.String()) {
		return 1
	}
	return 0
}

// text prints what get asks the daemon for: history or ls.
func text(dir string, get func(*rpc.Client) (string, error), stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	h, err := get(c)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	io.WriteString(stdout, h)
	return 0
}

// stop stops the daemon, whatever protocol it speaks.
func stop(dir string, stderr io.Writer) int {
	c, err := rpc.Dial(dir, nil)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		fmt.Fprintln(stderr, "whiff isn't running")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := c.Stop(); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}

func runDaemon(dir, services string, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := daemon.Run(ctx, dir, services); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}

// startDaemon starts `whiff daemon` in its own session, detached from the
// terminal, in / so it doesn't keep a client's directory busy.
func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
