package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/daemon"
)

// puff's hints name whiff's functions as whiff has them.
func TestWhiffFunctions(t *testing.T) {
	if got := daemon.Functions(); !slices.Equal(whiffFunctions, got) {
		t.Errorf("whiffFunctions = %q, want whiff's: %q", whiffFunctions, got)
	}
	var stderr strings.Builder
	if code := run([]string{"-e", "(call :a/b)"}, nil, io.Discard, &stderr, false, nil); code == 0 ||
		!strings.Contains(stderr.String(), "unknown name call: it is whiff's") {
		t.Errorf("puff -e (call :a/b) = %d %q", code, stderr.String())
	}
}

// TestExamples runs the tests of the plain puff files in examples, as puff
// -t does; services there are whiff's to test.
func TestExamples(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*.puff", "*/*.puff"} {
		found, _ := filepath.Glob(filepath.Join("..", "..", "examples", pattern))
		for _, f := range found {
			if src, err := os.ReadFile(f); err == nil && !strings.Contains(string(src), "(service ") {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("no examples")
	}
	var stdout, stderr strings.Builder
	if code := run(append([]string{"-t"}, files...), nil, &stdout, &stderr, false, nil); code != 0 {
		t.Errorf("puff -t %v = %d\n%s%s", files, code, stdout.String(), stderr.String())
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	file := write("t.puff", "(def x ; the answer\n  42)\n(+ x 1)\n")
	faulty := write("fault.puff", "(def x 1)\n(+ x 1.0)\n")
	withMain := write("main.puff", `(def (main args) (io/println "args:" args) (ok :done))`)
	noParams := write("noparams.puff", `(def (main) (ok 1))`)
	errMain := write("err.puff", `(def (main args) (err {:kind :bad}))`)
	oddMain := write("odd.puff", `(def (main args) 5)`)
	faultMain := write("faultmain.puff", "(def (main args)\n  (+ 1 1.0))")
	tested := write("tested.puff", "(def (sq n) (* n n))\n(test \"sq\" (= (sq 3) 9))\n(test \"wrong\" (= (sq 3) 6))\n(test \"fault\" (= (sq \"x\") 1))\n")
	passing := write("passing.puff", "(test \"one\" (= 1 1))\n")
	messy := write("messy.puff", "(def (sq n)\n(* n n))\n(test \"sq\" (= (sq 3) 9))\n")

	tests := []struct {
		name             string
		args             []string
		stdin            string
		code             int
		wantOut, wantErr string
	}{
		{
			name:    "-e",
			args:    []string{"-e", "(def (sq n) (* n n)) [(sq 3) 60s {:b 2 :a 1}]"},
			wantOut: "[9 1m {:a 1 :b 2}]\n",
		},
		{
			name:    "-e read error",
			args:    []string{"-e", "(a"},
			code:    1,
			wantErr: "-e:1:1: no closing ')'\n",
		},
		{
			name:    "-e unknown name",
			args:    []string{"-e", "(nope)"},
			code:    1,
			wantErr: "-e:1:2: unknown name nope\n",
		},
		{
			name:    "-e fault",
			args:    []string{"-e", "(+ 1 1.0)"},
			code:    70,
			wantErr: "-e:1:1: +: can't add int and float\n",
		},
		{
			name: "file without main",
			args: []string{file},
		},
		{
			name:    "file fault",
			args:    []string{faulty},
			code:    70,
			wantErr: faulty + ":2:1: +: can't add int and float\n",
		},
		{
			name:    "main with args",
			args:    []string{withMain, "a", "b c"},
			wantOut: "args: [\"a\" \"b c\"]\n",
		},
		{
			name: "main without params",
			args: []string{noParams, "ignored"},
		},
		{
			name:    "main returns err",
			args:    []string{errMain},
			code:    1,
			wantErr: "puff: (err {:kind :bad})\n",
		},
		{
			name:    "main returns something else",
			args:    []string{oddMain},
			code:    70,
			wantErr: "puff: main returned 5, want (ok v) or (err e)\n",
		},
		{
			name:    "main faults",
			args:    []string{faultMain},
			code:    70,
			wantErr: faultMain + ":2:3: +: can't add int and float\n",
		},
		{
			name:    "example",
			args:    []string{"../../examples/hello.puff", "ann", "bob"},
			wantOut: "hello, ann\nhello, bob\n",
		},
		{
			name:    "tests",
			args:    []string{"-t", passing, tested},
			code:    1,
			wantOut: "FAIL " + tested + ":3:1 wrong: got 9, want 6\nFAIL " + tested + ":4:1 fault: " + tested + ":1:13: *: can't multiply string and string\n2 passed, 2 failed\n",
		},
		{
			name:    "tests that pass",
			args:    []string{"-t", passing},
			wantOut: "1 passed, 0 failed\n",
		},
		{
			name:    "tests of an unformatted file",
			args:    []string{"-t", messy},
			code:    1,
			wantOut: "FAIL " + messy + ": not formatted; puff -f " + messy + "\n1 passed, 1 failed\n",
		},
		{
			name:    "tests of a broken file",
			args:    []string{"-t", faulty},
			code:    1,
			wantOut: "0 passed, 1 failed\n",
			wantErr: faulty + ":2:1: +: can't add int and float\n",
		},
		{
			name:    "missing file",
			args:    []string{"nope.puff"},
			code:    1,
			wantErr: "puff: open nope.puff: no such file or directory\n",
		},
		{
			name:    "stdin",
			stdin:   "(def x\n 2) (* x 3)\n(x ]\n\"one\ntwo\"\n(+ x 1.0)\n[1\n",
			wantOut: "6\n\"one\\ntwo\"\n",
			wantErr: "1:4: unexpected ']'\n1:1: +: can't add int and float\n1:1: no closing ']'\n",
		},
		{
			name:    "bad flag",
			args:    []string{"-x"},
			code:    2,
			wantErr: usage,
		},
		{
			name:    "-e without code",
			args:    []string{"-e"},
			code:    2,
			wantErr: usage,
		},
	}
	for _, tt := range tests {
		var out, errOut strings.Builder
		code := run(tt.args, strings.NewReader(tt.stdin), &out, &errOut, false, nil)
		if code != tt.code || out.String() != tt.wantOut || errOut.String() != tt.wantErr {
			t.Errorf("%s: got %d, stdout %q, stderr %q; want %d, %q, %q",
				tt.name, code, out.String(), errOut.String(), tt.code, tt.wantOut, tt.wantErr)
		}
	}
}

func TestFormat(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	messy := write("messy.puff", "(def (f x)\n(g x)\n)\n")
	tidy := write("tidy.puff", "(def (f x)\n  (g x))\n")
	broken := write("broken.puff", "(def x")
	puff := func(args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := run(args, strings.NewReader(""), &out, &errOut, false, nil)
		return code, out.String(), errOut.String()
	}

	// -l names what -f would change, and changes nothing.
	if code, out, errOut := puff("-l", messy, tidy, broken); code != 1 || out != messy+"\n" || errOut != broken+":1:1: no closing ')'\n" {
		t.Errorf("-l = %d %q %q", code, out, errOut)
	}
	if code, out, errOut := puff("-f", messy, tidy); code != 0 || out != messy+"\n" || errOut != "" {
		t.Errorf("-f = %d %q %q", code, out, errOut)
	}
	if b, _ := os.ReadFile(messy); string(b) != "(def (f x)\n  (g x))\n" {
		t.Errorf("messy.puff after -f = %q", b)
	}
	if code, out, _ := puff("-l", messy, tidy); code != 0 || out != "" {
		t.Errorf("-l after -f = %d %q", code, out)
	}
}

func TestInterrupt(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt
	var out, errOut strings.Builder
	code := run([]string{"-e", "(loop [i 0] (recur i))"}, strings.NewReader(""), &out, &errOut, false, interrupts)
	if want := "-e:1:1: cancelled\n"; code != 70 || errOut.String() != want {
		t.Errorf("got %d, stderr %q; want 70, %q", code, errOut.String(), want)
	}
}
