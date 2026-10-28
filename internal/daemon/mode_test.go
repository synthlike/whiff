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
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"relay": `(service relay
  (on (add text) "Adds text to notes." (call :notes/add text)))`,
		"tally": `(service tally
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
	for _, want := range []string{"note  :agent/1 is trusted :notes", "note  :agent/1 is free", "note  :agent/1 is gated", "note  :agent/1's free mode ran out: it is gated"} {
		if !strings.Contains(h, want) {
			t.Errorf("history has no %q", want)
		}
	}
}

func TestSession(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"tally": `(service tally
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
	do := func(c *rpc.Client, op, target, args string) {
		t.Helper()
		if _, err := c.Message(op, target, args, ""); err != nil {
			t.Fatalf("%s %s: %v", op, target, err)
		}
	}
	eval := func(src string) {
		t.Helper()
		if resp, err := agent.Eval(src, ""); err != nil || resp.Error != "" {
			t.Fatalf("%s: %+v %v", src, resp, err)
		}
	}

	do(agent, "call", "notes/add", `["a"]`) // held 1, accepted
	do(you, "accept", "", "[1]")
	do(agent, "call", "notes/add", `["b"]`) // held 2, rejected
	do(you, "reject", "", `[2 "no"]`)
	do(agent, "call", "notes/add", `["c"]`) // held 3, waits
	eval(`(call :notes/add "e")`)           // held 4, waits
	if _, err := you.Mode("agent/1", "trusted", []string{"notes"}, 0); err != nil {
		t.Fatal(err)
	}
	do(agent, "call", "notes/add", `["d"]`)
	do(agent, "call", "notes/list", `[]`) // a read: not in the session
	if _, err := you.Mode("agent/1", "free", nil, 0); err != nil {
		t.Fatal(err)
	}
	eval(`(file/append "` + file + `" "f")`)
	eval(`(publish :t/x "g")`)
	eventually(t, "the writes", func() string { b, _ := os.ReadFile(file); return string(b) }, "adfg")

	es, err := you.History()
	if err != nil {
		t.Fatal(err)
	}
	got := rpc.SessionText(es, ":agent/1")
	for _, want := range []string{
		`call  agent  :agent/1 -> :notes/add "a"  (err {:id 1 :kind :pending`, "[held 1]",
		`call  agent  :agent/1 -> :notes/add "a"  (ok :done)`, "[accepted 1]",
		`done  agent  :notes -> :agent/1/whiff/done 2 (err {:kind :rejected`,
		`eval  agent  :agent/1 -> eval "(call :notes/add \"e\")"`, "[held 4]",
		"note  :agent/1 is trusted :notes",
		`:agent/1 -> :notes/add "d"  (ok :done)`, "[trusted]",
		`eval  agent  :agent/1 -> eval "(file/append`, "[free]",
		`eval  agent  :agent/1 -> eval "(publish :t/x \"g\")"  (ok :done)`,
		"agent/1: 4 writes went through (1 trusted, 2 free, 1 accepted); 1 rejected; 2 wait for review\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("session has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "notes/list") {
		t.Errorf("session has a read:\n%s", got)
	}
	if got := rpc.SessionText(es, "agent/9"); got != "agent/9: no writes in history\n" {
		t.Errorf("an unknown session = %q", got)
	}
}
