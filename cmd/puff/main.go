// Command puff runs puff code without a whiff daemon. For now it reads code
// and prints its forms back, normalized; evaluation comes next.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

const usage = `usage:
  puff file.puff   read a file and print its forms
  puff -e code     read code and print its forms
  puff             read forms from stdin as they're typed
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
		return show("-e", args[1], stdout, stderr)
	case strings.HasPrefix(args[0], "-"):
		fmt.Fprint(stderr, usage)
		return 2
	}
	src, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "puff: %v\n", err)
		return 1
	}
	return show(args[0], string(src), stdout, stderr)
}

// show prints the forms in src, one per line.
func show(file, src string, stdout, stderr io.Writer) int {
	forms, err := puff.Read(file, src)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, f := range forms {
		fmt.Fprintln(stdout, f)
	}
	return 0
}

// repl reads stdin a line at a time and prints the forms read so far; it
// keeps reading while a form or string is still open.
func repl(stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
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
		forms, err := puff.Read("", buf.String())
		var perr *puff.Error
		if errors.As(err, &perr) && perr.Unclosed {
			continue
		}
		buf.Reset()
		if err != nil {
			fmt.Fprintln(stderr, err)
			continue
		}
		for _, f := range forms {
			fmt.Fprintln(stdout, f)
		}
	}
	if interactive {
		fmt.Fprintln(stdout)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "puff: %v\n", err)
		return 1
	}
	if buf.Len() > 0 {
		_, err := puff.Read("", buf.String())
		fmt.Fprintln(stderr, err)
	}
	return 0
}
