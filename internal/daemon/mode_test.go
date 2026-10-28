package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestModes(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(srv notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"relay": `(srv relay
  (on (add text) "Adds text to notes." (call :notes/add text)))`,
		"tally": `(srv tally
  (on :t/x (v) (call :notes/add v)))`,
	})
	start(t, dir)
	you := dial(t, dir)
	if _, err := you.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	agent := dial(t, dir)
	agent.Agent = true
	if resp, err := agent.Claim("agent", `(ask (tell text) "Asks the agent.")`); err != nil || resp.Value != "(ok :agent/1)" {
		t.Fatalf("claim = %+v %v", resp, err)
	}

	// call is what the agent's call of target with args gets: ok, or the
	// kind of its err.
	call := func(target, args string) string {
		t.Helper()
		resp, err := agent.Message("call", target, args, "")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if strings.HasPrefix(resp.Value, "(ok") {
			return "ok"
		}
		_, kind, _ := strings.Cut(resp.Value, ":kind ")
		kind, _, _ = strings.Cut(kind, " ")
		return kind
	}
	eval := func(src string) string {
		t.Helper()
		resp, err := agent.Eval(src, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	mode := func(c *rpc.Client, mode string, services []string, d time.Duration) string {
		t.Helper()
		resp, err := c.Mode("agent/1", mode, services, d)
		if err != nil {
			return "error: " + err.Error()
		}
		return resp.Value
	}
	expect := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %s, want %s", what, got, want)
		}
	}

	expect("gated add", call("notes/add", `["a"]`), ":pending")
	expect("the agent setting its mode", mode(agent, "free", nil, 0), "error: an agent can't set a session's mode")
	expect("trusting nobody", mode(you, "trusted", []string{"nobody"}, 0), `(err {:kind :no-address :message "no address :nobody to trust"})`)
	if r, err := you.Mode("agent/9", "free", nil, 0); err != nil || !strings.Contains(r.Value, ":no-address") {
		t.Errorf("free for no session = %+v %v", r, err)
	}

	// Trusted: messages to notes go through; to relay, which isn't
	// trusted, they still wait.
	expect("trust notes", mode(you, "trusted", []string{"notes"}, 0), "(ok :done)")
	expect("trusted add", call("notes/add", `["b"]`), "ok")
	expect("untrusted relay", call("relay/add", `["c"]`), ":pending")
	// What a trusted service does for the agent goes through too.
	expect("trust relay", mode(you, "trusted", []string{"relay"}, 0), "(ok :done)")
	expect("trusted relay", call("relay/add", `["d"]`), "ok")
	// An eval that reaches :write still waits, and so does a publish.
	expect("trusted eval", eval(`(match (call :notes/list) [(err {:kind k}) k] [_ :ran])`), ":ran")
	for _, src := range []string{`(call :notes/add "e")`, `(publish :t/x "e")`} {
		if got := eval(src); !strings.Contains(got, "the agent's eval waits for your review") {
			t.Errorf("trusted %s = %s, want it held", src, got)
		}
	}
	if ls, _ := you.Ls(); !strings.Contains(ls, ":agent/1 (agent, trusted :relay)") {
		t.Errorf("ls = %s, want the session's mode", ls)
	}

	// Free: everything goes through, evals and publishes included.
	expect("free", mode(you, "free", nil, 0), "(ok :done)")
	expect("free eval", eval(`(file/append "`+file+`" "f")`), "(ok :done)")
	expect("free publish", eval(`(publish :t/x "g")`), "(ok :done)")
	eventually(t, "the free session's writes", func() string { b, _ := os.ReadFile(file); return string(b) }, "bdfg")

	// Gate puts it back, and a mode for a while runs out.
	expect("gate", mode(you, "gated", nil, 0), "(ok :done)")
	expect("gated again", call("notes/add", `["h"]`), ":pending")
	expect("free for 1s", mode(you, "free", nil, time.Second), "(ok :done)")
	if ls, _ := you.Ls(); !strings.Contains(ls, ":agent/1 (agent, free, 1s left)") {
		t.Errorf("ls = %s, want free with 1s left", ls)
	}
	expect("free for a while", call("notes/add", `["i"]`), "ok")
	eventually(t, "the mode running out", func() string {
		ls, _ := you.Ls()
		if strings.Contains(ls, ":agent/1 (agent, gated)") {
			return "gated"
		}
		return ls
	}, "gated")
	expect("after the mode ran out", call("notes/add", `["j"]`), ":pending")

	h, err := historyOf(you)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"note  :agent/1 is trusted :notes", "note  :agent/1 is free", "note  :agent/1 is gated"} {
		if !strings.Contains(h, want) {
			t.Errorf("history has no %q", want)
		}
	}
}
