// Command puff runs puff code without a whiff daemon.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

const usage = `usage:
  puff file.puff   evaluate a file
  puff -e code     evaluate code and print its value
  puff             evaluate forms from stdin as they're typed
`

func main() {
	fi, err := os.Stdin.Stat()
	interactive := err == nil && fi.Mode()&os.ModeCharDevice != 0
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, interactive))
}

// run is puff with its arguments and streams, returning the exit code.
// interactive shows a prompt on stdout.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	switch {
	case len(args) == 0:
		return repl(stdin, stdout, stderr, interactive)
	case args[0] == "-h" || args[0] == "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case args[0] == "-e" && len(args) == 2:
		return eval(puff.NewEnv(), "-e", args[1], stdout, stderr, true)
	case strings.HasPrefix(args[0], "-"):
		fmt.Fprint(stderr, usage)
		return 2
	}
	src, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "puff: %v\n", err)
		return 1
	}
	return eval(puff.NewEnv(), args[0], string(src), stdout, stderr, false)
}

// eval loads src into env and, if show is set, prints its value. A fault
// exits 70, any other error 1.
func eval(env *puff.Env, file, src string, stdout, stderr io.Writer, show bool) int {
	v, err := env.Load(context.Background(), file, src)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var fault *puff.Fault
		if errors.As(err, &fault) {
			return 70
		}
		return 1
	}
	if show && v != nil {
		fmt.Fprintln(stdout, puff.Print(v))
	}
	return 0
}

// repl reads stdin a line at a time and evaluates what it has read, printing
// the value; it keeps reading while a form or string is still open.
func repl(stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	env := puff.NewEnv()
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
		eval(env, "", buf.String(), stdout, stderr, true)
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
		eval(env, "", buf.String(), stdout, stderr, true)
	}
	return 0
}
