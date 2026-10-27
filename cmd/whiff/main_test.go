package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain runs whiff itself when the tests start it as the daemon, on the
// directory they pass in WHIFF_TEST_DIR.
func TestMain(m *testing.M) {
	if dir := os.Getenv("WHIFF_TEST_DIR"); dir != "" {
		os.Exit(run(os.Args[1:], dir, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestRun(t *testing.T) {
	// A short directory: a unix socket path must fit in about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("WHIFF_TEST_DIR", dir)
	t.Cleanup(func() { run([]string{"stop"}, dir, new(strings.Builder), new(strings.Builder)) })

	tests := []struct {
		name             string
		args             []string
		code             int
		wantOut, wantErr string
	}{
		{name: "not running", args: []string{"stop"}, wantErr: "whiff isn't running\n"},
		{name: "eval starts the daemon", args: []string{"eval", `(io/println "hi") (+ 1 2)`}, wantOut: "hi\n3\n"},
		{name: "read error", args: []string{"eval", "(a"}, code: 1, wantErr: "eval:1:1: no closing ')'\n"},
		{name: "fault", args: []string{"eval", "(+ 1 1.0)"}, code: 70, wantErr: "eval:1:1: +: can't add int and float\n"},
		{name: "stop", args: []string{"stop"}},
		{name: "stopped", args: []string{"stop"}, wantErr: "whiff isn't running\n"},
		{name: "eval starts it again", args: []string{"eval", "(os/cwd)"}, wantOut: quote(t) + "\n"},
		{name: "usage", args: []string{"nope"}, code: 2, wantErr: usage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := run(tt.args, dir, &stdout, &stderr)
			if code != tt.code || stdout.String() != tt.wantOut || stderr.String() != tt.wantErr {
				t.Errorf("run(%q) = %d\nstdout %q\nstderr %q\nwant %d, %q, %q",
					tt.args, code, stdout.String(), stderr.String(), tt.code, tt.wantOut, tt.wantErr)
			}
		})
	}
}

// quote is the test's working directory as a printed puff string.
func quote(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return `"` + wd + `"`
}
