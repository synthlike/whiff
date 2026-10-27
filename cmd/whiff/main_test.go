package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain runs whiff itself when the tests start it as the daemon, on the
// directory they pass in WHIFF_TEST_DIR, with services in its services/.
func TestMain(m *testing.M) {
	if dir := os.Getenv("WHIFF_TEST_DIR"); dir != "" {
		os.Exit(run(os.Args[1:], dir, filepath.Join(dir, "services"), os.Stdout, os.Stderr))
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
	services := filepath.Join(dir, "services")
	os.Mkdir(services, 0o700)
	greet := `(srv greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name))))`
	if err := os.WriteFile(filepath.Join(services, "greet.puff"), []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHIFF_TEST_DIR", dir)
	t.Cleanup(func() { run([]string{"stop"}, dir, services, new(strings.Builder), new(strings.Builder)) })

	tests := []struct {
		name             string
		args             []string
		code             int
		wantOut, wantErr string
	}{
		{name: "not running", args: []string{"stop"}, wantErr: "whiff isn't running\n"},
		{name: "eval starts the daemon", args: []string{"eval", `(io/println "hi") (+ 1 2)`}, wantOut: "hi\n3\n"},
		{name: "call a service", args: []string{"eval", `(call :greet/hello "ann")`}, wantOut: "(ok \"hello, ann\")\n"},
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
			code := run(tt.args, dir, services, &stdout, &stderr)
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
