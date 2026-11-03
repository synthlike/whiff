// Command puff runs puff code without a whiff daemon.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"

	"golang.org/x/term"

	"github.com/synthlike/whiff/internal/puff"
)

const usage = `usage:
  puff file.puff args...   evaluate a file and call its main with args
  puff -e code             evaluate code and print its value
  puff -t file.puff...     run the files' tests
  puff -f file.puff...     format the files in place, naming each it changes
  puff -l file.puff...     name the files that aren't formatted
  puff                     evaluate forms from stdin as they're typed
`

func main() {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, terminal(os.Stdin), interrupts))
}

// terminal says whether f is a terminal: /dev/null is a character device
// too, but gets no prompt.
func terminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// run is puff with its arguments and streams, returning the exit code.
// interactive shows a prompt on stdout; an interrupt cancels what is being
// evaluated.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool, interrupts <-chan os.Signal) int {
	switch {
	case len(args) == 0:
		return repl(stdin, stdout, stderr, interactive, interrupts)
	case args[0] == "-h" || args[0] == "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case args[0] == "-e" && len(args) == 2:
		ctx, cancel := cancelOn(interrupts)
		defer cancel()
		return eval(ctx, newEnv(stdout), "-e", args[1], stdout, stderr, true)
	case args[0] == "-t" && len(args) >= 2:
		ctx, cancel := cancelOn(interrupts)
		defer cancel()
		return test(ctx, args[1:], stdout, stderr)
	case (args[0] == "-f" || args[0] == "-l") && len(args) >= 2:
		return format(args[1:], args[0] == "-f", stdout, stderr)
	case strings.HasPrefix(args[0], "-"):
		fmt.Fprint(stderr, usage)
		return 2
	}
	ctx, cancel := cancelOn(interrupts)
	defer cancel()
	return runFile(ctx, args[0], args[1:], stdout, stderr)
}

// cancelOn returns a context that the first interrupt cancels.
func cancelOn(interrupts <-chan os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if interrupts != nil {
		go func() {
			select {
			case <-interrupts:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// whiffFunctions are whiff's functions, which puff hasn't: a test keeps them
// whiff's, without the puff binary depending on whiff.
var whiffFunctions = []string{
	"address/describe", "address/list", "call", "hand-on", "inbox", "publish", "reply", "self",
	"send", "send-after", "service/list", "service/reset", "service/state", "service/status", "session/mode",
	"submitted", "view/board", "view/items", "view/locations", "view/text",
}

func newEnv(stdout io.Writer) *puff.Env {
	env := puff.NewEnv()
	env.Stdout = stdout
	env.Hints = map[string]string{}
	for _, name := range whiffFunctions {
		env.Hints[name] = "it is whiff's: run this in a service, or with whiff eval"
	}
	return env
}

// runFile evaluates a file and, if it defines main, calls it with args as a
// vector of strings (or with nothing, if main takes no parameters). (ok v)
// exits 0, (err e) 1, a fault 70.
func runFile(ctx context.Context, file string, args []string, stdout, stderr io.Writer) int {
	src, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "puff: %v\n", err)
		return 1
	}
	env := newEnv(stdout)
	if _, err := env.LoadFile(ctx, file, string(src)); err != nil {
		return report(stderr, err)
	}
	main, ok := env.Lookup("main")
	if !ok {
		return 0
	}
	var mainArgs []puff.Value
	if f, ok := main.(*puff.Func); !ok || f.Arity() != 0 {
		vs := make([]puff.Value, len(args))
		for i, a := range args {
			vs[i] = puff.String(a)
		}
		mainArgs = []puff.Value{puff.NewVector(vs...)}
	}
	v, err := env.Call(ctx, puff.Pos{File: file, Line: 1, Col: 1}, main, mainArgs...)
	if err != nil {
		return report(stderr, err)
	}
	if t, ok := v.(puff.Tag); ok && t.Len() == 1 {
		switch t.Name() {
		case "ok":
			return 0
		case "err":
			fmt.Fprintf(stderr, "puff: %s\n", puff.Print(v))
			return 1
		}
	}
	fmt.Fprintf(stderr, "puff: main returned %s, want (ok v) or (err e)\n", puff.Print(v))
	return 70
}

// test loads each file and runs its tests, printing each that fails and
// how many passed: 0 if they all did, 1 if any failed or a file didn't
// load. A file that isn't formatted, as puff -f would, fails too.
func test(ctx context.Context, files []string, stdout, stderr io.Writer) int {
	passed, failed := 0, 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "puff: %v\n", err)
			failed++
			continue
		}
		// A file that doesn't read is left for Load to say why.
		if out, err := puff.Format(file, string(src)); err == nil && out != string(src) {
			fmt.Fprintf(stdout, "FAIL %s: not formatted; puff -f %s\n", file, file)
			failed++
		}
		env := newEnv(stdout)
		if _, err := env.LoadFile(ctx, file, string(src)); err != nil {
			fmt.Fprintln(stderr, err)
			failed++
			continue
		}
		for _, t := range env.Tests() {
			if msg := env.RunTest(ctx, t); msg != "" {
				fmt.Fprintf(stdout, "FAIL %s %s: %s\n", t.Pos, t.Name, msg)
				failed++
			} else {
				passed++
			}
		}
	}
	fmt.Fprintf(stdout, "%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// format formats files, puff.Format's way, and names each that wasn't
// formatted; write rewrites them, else it is 1 if any wasn't. A file that
// doesn't read is an error and left as it is.
func format(files []string, write bool, stdout, stderr io.Writer) int {
	code := 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "puff: %v\n", err)
			code = 1
			continue
		}
		out, err := puff.Format(file, string(src))
		switch {
		case err != nil:
			fmt.Fprintln(stderr, err)
			code = 1
			continue
		case out == string(src):
			continue
		case !write:
			code = 1
		default:
			if err := os.WriteFile(file, []byte(out), 0o644); err != nil {
				fmt.Fprintf(stderr, "puff: %v\n", err)
				code = 1
				continue
			}
		}
		fmt.Fprintln(stdout, file)
	}
	return code
}

// report prints a read, check or evaluation error and returns its exit
// code: 70 for a fault, 1 for the rest.
func report(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, err)
	var fault *puff.Fault
	if errors.As(err, &fault) {
		return 70
	}
	return 1
}

// eval loads src into env and, if show is set, prints its value.
func eval(ctx context.Context, env *puff.Env, file, src string, stdout, stderr io.Writer, show bool) int {
	v, err := env.Load(ctx, file, src)
	if err != nil {
		return report(stderr, err)
	}
	if show && v != nil {
		fmt.Fprintln(stdout, puff.Print(v))
	}
	return 0
}

// repl reads stdin a line at a time and evaluates what it has read, printing
// the value; it keeps reading while a form or string is still open. An
// interrupt cancels the evaluation, or ends the REPL at its prompt.
func repl(stdin io.Reader, stdout, stderr io.Writer, interactive bool, interrupts <-chan os.Signal) int {
	var running atomic.Pointer[context.CancelFunc]
	if interrupts != nil {
		go func() {
			for range interrupts {
				if cancel := running.Load(); cancel != nil {
					(*cancel)()
					continue
				}
				fmt.Fprintln(stdout)
				os.Exit(130)
			}
		}()
	}
	evalInput := func(env *puff.Env, src string) {
		ctx, cancel := context.WithCancel(context.Background())
		running.Store(&cancel)
		eval(ctx, env, "", src, stdout, stderr, true)
		running.Store(nil)
		cancel()
	}

	env := newEnv(stdout)
	sc := bufio.NewScanner(stdin)
	var buf strings.Builder
	for {
		if interactive {
			if buf.Len() == 0 {
				fmt.Fprint(stdout, "puff> ")
			} else {
				fmt.Fprint(stdout, "  ... ")
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
		evalInput(env, buf.String())
		buf.Reset()
	}
	if interactive {
		fmt.Fprintln(stdout)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "puff: %v\n", err)
		return 1
	}
	if buf.Len() > 0 {
		evalInput(env, buf.String())
	}
	return 0
}
