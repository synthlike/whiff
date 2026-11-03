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
	greet := `(service greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name)))
  (on (echo x) "Returns x."
    (ok x))
  (on (pick) "Items to pick."
    (ok (view/items [{:label "a" :action [:greet/hello "x"]} {:label "b\tc" :detail "d"} {:label "e" :url "http://localhost:8080"}])))
  (on (places) "Locations."
    (ok (view/locations [{:file "a.go" :line 3 :col 2 :note "here"} {:file "b.go"}])))
  (on (say) "Text."
    (ok (view/text "two\nlines" {:changed :greet/changed})))
  (on (board) "A board."
    (ok (view/board [{:title "Now" :items [{:label "a" :actions [{:label "Greet x" :action [:greet/hello "x"]} {:label "Greet someone" :action [:greet/hello] :ask "Who?"}]}]}
                     {:title "Later" :items [{:label "b"}]}])))
  (on (stuck) "Fails, offering what to do instead."
    (err {:kind :not-now :actions [{:label "Greet x" :action [:greet/hello "x"]} {:label "Greet someone" :action [:greet/hello] :ask "Who?"}]})))`
	if err := os.WriteFile(filepath.Join(services, "greet.puff"), []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	// A greet being tried, beside the installed one.
	trial, broken := filepath.Join(dir, "greet.puff"), filepath.Join(dir, "x", "greet.puff")
	os.Mkdir(filepath.Join(dir, "x"), 0o700)
	if err := os.WriteFile(trial, []byte(`(service greet (on (hello name) "Greets name." (ok (string/concat "hi, " name))))`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("(service greet\n  (on (hello) \"Doc.\" (ok x)))\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tested := filepath.Join(dir, "x", "tested.puff")
	if err := os.WriteFile(tested, []byte("(service tested\n  (def (sq n) (* n n))\n  (test \"sq\" (= (sq 3) 9))\n  (test \"wrong\" (= (sq 3) 6)))\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	messy := filepath.Join(dir, "x", "messy.puff")
	if err := os.WriteFile(messy, []byte("(service messy\n(test \"one\" (= 1 1)))\n"), 0o600); err != nil {
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
		{name: "an err's actions", args: []string{"call", "greet/stuck"}, code: 1,
			wantErr: "whiff: {:kind :not-now}\n  Greet x\t[:greet/hello \"x\"]\n  Greet someone\t[:greet/hello \"\"]\n"},
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
		{name: "items", args: []string{"call", "greet/pick"}, wantOut: "a\t\t[:greet/hello \"x\"]\nb c\td\ne\thttp://localhost:8080\n"},
		{name: "locations", args: []string{"call", "greet/places"}, wantOut: "a.go:3:2: here\nb.go\n"},
		{name: "text", args: []string{"call", "greet/say"}, wantOut: "two\nlines\n"},
		{name: "board", args: []string{"call", "greet/board"}, wantOut: "# Now\na\t\tGreet x\t[:greet/hello \"x\"]\na\t\tGreet someone\t[:greet/hello \"\"]\n\n# Later\nb\n"},
		{name: "act on an action's line", args: []string{"act", "a\t\tGreet y\t[:greet/hello \"y\"]"}, wantOut: "hello, y\n"},
		{name: "act on a line", args: []string{"act", "a\t\t[:greet/hello \"x\"]"}, wantOut: "hello, x\n"},
		{name: "act on stdin", args: []string{"act"}, stdin: "a\t\t[:greet/hello \"x\"]\n[:greet/hello \"y\"]\n", wantOut: "hello, x\nhello, y\n"},
		{name: "act without an action", args: []string{"act", "b c\td"}, code: 1,
			wantErr: "whiff: \"b c\\td\" has no action, want [:address/fn args...] at its end\n"},
		{name: "try", args: []string{"try", trial, "hello", "dee"}, wantOut: "hi, dee\n"},
		{name: "installed after try", args: []string{"call", "greet/hello", "dee"}, wantOut: "hello, dee\n"},
		{name: "try err", args: []string{"try", trial, "nope"}, code: 1,
			wantErr: "whiff: {:kind :no-function :message \":try/2/greet has no function nope\"}\n"},
		{name: "try a broken file", args: []string{"try", broken, "hello"}, code: 1, wantErr: broken + ":2:26: unknown name x\n"},
		{name: "try a missing file", args: []string{"try", "nope.puff", "hello"}, code: 1,
			wantErr: "whiff: open nope.puff: no such file or directory\n"},
		{name: "test", args: []string{"test", trial, tested}, code: 1,
			wantOut: "FAIL " + tested + ":4:3 wrong: got 9, want 6\n1 passed, 1 failed\n"},
		{name: "test a broken file", args: []string{"test", broken}, code: 1, wantOut: "0 passed, 1 failed\n", wantErr: broken + ":2:26: unknown name x\n"},
		{name: "test an unformatted file", args: []string{"test", messy}, code: 1,
			wantOut: "FAIL " + messy + ": not formatted; puff -f " + messy + "\n1 passed, 1 failed\n"},
		{name: "your inbox, empty", args: []string{"inbox"}},
		{name: "call an ask", args: []string{"call", "me/ask", "lunch?"}, outHas: "pending "},
		{name: "your inbox", args: []string{"inbox"}, outHas: `  client -> :me/ask "lunch?"`},
		{name: "send", args: []string{"send", "greet/hello", "cat"}},
		{name: "send err", args: []string{"send", "nobody/x"}, code: 1,
			wantErr: "whiff: {:kind :no-address :message \"no address :nobody\"}\n"},
		{name: "history", args: []string{"history"}, outHas: `  call  user   client -> :greet/hello "bob"  (ok "hello, bob")  `},
		{name: "history with a query", args: []string{"history", "to:greet", "bob"}, outHas: `  call  user   client -> :greet/hello "bob"  (ok "hello, bob")  `},
		{name: "history as chains", args: []string{"history", "-c", "to:greet", "bob"}, outHas: `  call  user   client -> :greet/hello "bob"  (ok "hello, bob")  `},
		{name: "history of errors", args: []string{"history", "kind:err", "-to:greet"}, outHas: `  send  user   client -> :nobody/x  (err {:kind :no-address`},
		{name: "history matching nothing", args: []string{"history", "to:nobody", "bob"}},
		{name: "history with a bad query", args: []string{"history", "-f", "kind:big"}, code: 2,
			wantErr: "whiff: kind:big: want kind: call, send, pub, done, eval or note, or ok, err, fault or pending\n"},
		{name: "state of a service without state", args: []string{"state", "greet"}, code: 1,
			wantErr: "whiff: {:kind :stateless :message \":greet has no state\"}\n"},
		{name: "reset a service without state", args: []string{"reset", "greet"}, code: 1,
			wantErr: "whiff: {:kind :stateless :message \":greet has no state\"}\n"},
		{name: "free no session", args: []string{"free", "agent/9"}, code: 1,
			wantErr: "whiff: {:kind :no-address :message \"no agent session :agent/9\"}\n"},
		{name: "session with no writes", args: []string{"session", "agent/9"}, wantOut: "agent/9: no writes in history\n"},
		{name: "trust nothing", args: []string{"trust", "agent/9"}, code: 2, wantErr: usage},
		{name: "free for a bad while", args: []string{"free", "agent/9", "--for", "soon"}, code: 2, wantErr: usage},
		{name: "gate for a while", args: []string{"gate", "agent/9", "--for", "1h"}, code: 2, wantErr: usage},
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

// TestTerminal: only a terminal gets a prompt in whiff repl, not /dev/null
// (a character device too) or a pipe.
func TestTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if terminal(null) || terminal(r) {
		t.Errorf("terminal: /dev/null %v, a pipe %v; want false", terminal(null), terminal(r))
	}
	tty, err := os.Open("/dev/tty")
	if err != nil {
		t.Skip("no terminal:", err)
	}
	defer tty.Close()
	if !terminal(tty) {
		t.Error("terminal(/dev/tty) = false")
	}
}
