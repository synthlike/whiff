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
    (ok (string/concat "hello, " name)))
  (on (echo x) "Returns x."
    (ok x)))`
	if err := os.WriteFile(filepath.Join(services, "greet.puff"), []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	// A greet being tried, beside the installed one.
	trial, broken := filepath.Join(dir, "greet.puff"), filepath.Join(dir, "x", "greet.puff")
	os.Mkdir(filepath.Join(dir, "x"), 0o700)
	if err := os.WriteFile(trial, []byte(`(srv greet (on (hello name) "Greets name." (ok (string/concat "hi, " name))))`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("(srv greet\n  (on (hello) \"Doc.\" (ok x)))"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHIFF_TEST_DIR", dir)
	t.Cleanup(func() { run([]string{"stop"}, dir, services, new(strings.Builder), new(strings.Builder)) })

	tests := []struct {
		name             string
		args             []string
		code             int
		wantOut, wantErr string
		outHas           string // instead of wantOut, a line stdout has
		stdin            string // what whiff repl reads
	}{
		{name: "not running", args: []string{"stop"}, wantErr: "whiff isn't running\n"},
		{name: "eval starts the daemon", args: []string{"eval", `(io/println "hi") (+ 1 2)`}, wantOut: "hi\n3\n"},
		{name: "eval a call", args: []string{"eval", `(call :greet/hello "ann")`}, wantOut: "(ok \"hello, ann\")\n"},
		{name: "repl", args: []string{"repl"}, stdin: "(def (greet n) (call :greet/hello n))\n(greet\n  \"rae\")\n(+ 1 1.0)\n(io/println \"still\")\n",
			wantOut: "(ok \"hello, rae\")\nstill\n(ok :done)\n", wantErr: "repl:1:1: +: can't add int and float\n"},
		{name: "read error", args: []string{"eval", "(a"}, code: 1, wantErr: "eval:1:1: no closing ')'\n"},
		{name: "fault", args: []string{"eval", "(+ 1 1.0)"}, code: 70, wantErr: "eval:1:1: +: can't add int and float\n"},
		{name: "call", args: []string{"call", "greet/hello", "bob"}, wantOut: "hello, bob\n"},
		{name: "call with a colon", args: []string{"call", ":greet/hello", "bob"}, wantOut: "hello, bob\n"},
		{name: "call an int", args: []string{"call", "greet/echo", "12"}, wantOut: "12\n"},
		{name: "call a vector", args: []string{"call", "greet/echo", "[1 :a]"}, wantOut: "[1 :a]\n"},
		{name: "call err", args: []string{"call", "greet/nope"}, code: 1,
			wantErr: "whiff: {:kind :no-function :message \":greet has no function nope\"}\n"},
		{name: "call a bad target", args: []string{"call", "greet"}, code: 1,
			wantErr: "whiff: :greet has no address: only a service can leave it out, want :address/fn\n"},
		{name: "try", args: []string{"try", trial, "hello", "dee"}, wantOut: "hi, dee\n"},
		{name: "installed after try", args: []string{"call", "greet/hello", "dee"}, wantOut: "hello, dee\n"},
		{name: "try err", args: []string{"try", trial, "nope"}, code: 1,
			wantErr: "whiff: {:kind :no-function :message \":try/2/greet has no function nope\"}\n"},
		{name: "try a broken file", args: []string{"try", broken, "hello"}, code: 1, wantErr: broken + ":2:26: unknown name x\n"},
		{name: "try a missing file", args: []string{"try", "nope.puff", "hello"}, code: 1,
			wantErr: "whiff: open nope.puff: no such file or directory\n"},
		{name: "your inbox, empty", args: []string{"inbox"}},
		{name: "call an ask", args: []string{"call", "me/ask", "lunch?"}, outHas: "pending "},
		{name: "your inbox", args: []string{"inbox"}, outHas: `  client -> :me/ask "lunch?"`},
		{name: "send", args: []string{"send", "greet/hello", "cat"}},
		{name: "send err", args: []string{"send", "nobody/x"}, code: 1,
			wantErr: "whiff: {:kind :no-address :message \"no address :nobody\"}\n"},
		{name: "history", args: []string{"history"}, outHas: `  call  user   client -> :greet/hello "bob"  (ok "hello, bob")  `},
		{name: "stop", args: []string{"stop"}},
		{name: "stopped", args: []string{"stop"}, wantErr: "whiff isn't running\n"},
		{name: "eval starts it again", args: []string{"eval", "(os/cwd)"}, wantOut: quote(t) + "\n"},
		{name: "usage", args: []string{"nope"}, code: 2, wantErr: usage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			stdin = strings.NewReader(tt.stdin)
			code := run(tt.args, dir, services, &stdout, &stderr)
			out := stdout.String()
			if tt.outHas != "" && strings.Contains(out, tt.outHas) {
				out = ""
			}
			if code != tt.code || out != tt.wantOut || stderr.String() != tt.wantErr {
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
