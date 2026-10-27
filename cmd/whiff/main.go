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
	"github.com/synthlike/whiff/internal/rpc"
)

const usage = `usage:
  whiff eval code   evaluate puff code in the daemon and print its value
  whiff stop        stop the daemon
  whiff daemon      run the daemon in the foreground
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
	case len(args) == 2 && args[0] == "eval":
		return eval(dir, args[1], stdout, stderr)
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

// eval prints src's value as the daemon evaluates it: exit 0, 1 for an
// error, 70 for a fault, like puff -e.
func eval(dir, src string, stdout, stderr io.Writer) int {
	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := c.Compatible(); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
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
