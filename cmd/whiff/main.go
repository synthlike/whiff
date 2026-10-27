// Command whiff is the whiff daemon and its clients.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/synthlike/whiff/internal/daemon"
	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

const usage = `usage:
  whiff call address/fn args...   call a function and print its value
  whiff send address/fn args...   send a message without waiting
  whiff eval code                 evaluate puff code in the daemon and print its value
  whiff history                   print every message, newest first
  whiff stop                      stop the daemon
  whiff daemon                    run the daemon in the foreground

Each arg of call and send is read as a puff value, or taken as a string if
it doesn't read as one: 12 is 12, ann is "ann", '"12"' is "12".
`

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
	case len(args) == 2 && args[0] == "eval":
		return eval(dir, args[1], stdout, stderr)
	case len(args) == 1 && args[0] == "history":
		return history(dir, stdout, stderr)
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

// message calls or sends (op) target. A call prints the value of (ok v), a
// string as it is; (err e) prints e and exits 1. A send prints nothing.
func message(dir, op, target string, args []string, stdout, stderr io.Writer) int {
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
	resp, err := c.Message(op, target, puff.Print(puff.NewVector(vals...)), wd)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	v, err := puff.ReadValue(resp.Value)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 {
		fmt.Fprintf(stderr, "whiff: want a Result, got %s\n", resp.Value)
		return 1
	}
	switch {
	case r.Name() == "err":
		fmt.Fprintf(stderr, "whiff: %s\n", puff.Print(r.At(0)))
		return 1
	case op == "call":
		if s, ok := r.At(0).(puff.String); ok {
			fmt.Fprintln(stdout, string(s))
		} else {
			fmt.Fprintln(stdout, puff.Print(r.At(0)))
		}
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

func history(dir string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	h, err := c.History()
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
