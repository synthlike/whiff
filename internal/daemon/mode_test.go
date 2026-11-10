package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

func TestModes(t *testing.T) {
	t.Parallel()
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
	expect("the agent's mode", eval(`(session/mode)`), "(ok {:mode :gated})")
	if r, err := you.Eval(`(session/mode)`, ""); err != nil || r.Value != `(err {:kind :no-session :message "only an agent session has a mode"})` {
		t.Errorf("your (session/mode) = %+v %v", r, err)
	}

	// Trusted: messages to notes go through; to relay, which isn't
	// trusted, they still wait.
	expect("trust notes", mode(you, "trusted", []string{"notes"}, 0), "(ok :done)")
	expect("trusted add", call("notes/add", `["b"]`), "ok")
	expect("untrusted relay", call("relay/add", `["c"]`), ":pending")
	// What a trusted service does for the agent goes through too.
	expect("trust relay", mode(you, "trusted", []string{"relay"}, 0), "(ok :done)")
	expect("trusted relay", call("relay/add", `["d"]`), "ok")
	expect("the trusted mode", eval(`(session/mode)`), "(ok {:mode :trusted :services [:relay]})")
	// An eval whose only writes are messages to relay runs; one that
	// reaches another :write still waits, and so does a publish.
	expect("trusted eval", eval(`(match (call :notes/list) [(err {:kind k}) k] [_ :ran])`), ":ran")
	expect("trusted eval to relay", eval(`(call :relay/add "r")`), "(ok :done)")
	for _, src := range []string{`(call :notes/add "e")`, `(do (call :relay/add "e") (call :notes/add "e"))`, `(publish :t/x "e")`} {
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
	eventually(t, "the free session's writes", func() string { b, _ := os.ReadFile(file); return string(b) }, "bdrfg")

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
	for _, want := range []string{"note  :agent/1 is trusted :notes", "note  :agent/1 is free", "note  :agent/1 is gated", "note  :agent/1's free mode used up its time: it is gated again"} {
		if !strings.Contains(h, want) {
			t.Errorf("history has no %q", want)
		}
	}
}

func TestSession(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
		"tally": `(service tally
  (on :t/x (v) (call :notes/add v)))`,
		"relay": `(service relay
  (on (add text) "Adds text to the notes." (call :notes/add text)))`,
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
	if _, err := you.Mode("agent/1", "trusted", []string{"notes", "relay"}, 0); err != nil {
		t.Fatal(err)
	}
	do(agent, "call", "notes/add", `["d"]`)
	do(agent, "call", "relay/add", `["h"]`) // and :relay's write for it
	do(agent, "call", "notes/list", `[]`)   // a read: not in the session
	if _, err := you.Mode("agent/1", "free", nil, 0); err != nil {
		t.Fatal(err)
	}
	eval(`(file/append "` + file + `" "f")`)
	eval(`(publish :t/x "g")`)
	eventually(t, "the writes", func() string { b, _ := os.ReadFile(file); return string(b) }, "adhfg")
	// :tally's call is in history once :notes has handled it, after the write.
	historyHas(t, you, `:tally -> :notes/add "g"  (ok :done)`)

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
		`:agent/1 -> :relay/add "h"  (ok :done)`, "[trusted]",
		`:relay -> :notes/add "h"  (ok :done)`, "[for agent/1]",
		`eval  agent  :agent/1 -> eval "(publish :t/x \"g\")"  (ok :done)`,
		`:tally -> :notes/add "g"  (ok :done)`,
		"agent/1: 5 writes went through (2 trusted, 2 free, 1 accepted), and 2 by services for it; 1 rejected; 2 wait for review\n",
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

// TestStandingApproval trusts an agent session for one function, for so
// many writes: a standing approval. Its writes there go through, counted;
// reads aren't counted, and the service's other writes still wait. Once
// it has let through as many as it may, the session is gated again, and
// your inbox says so. A function that isn't there can't be trusted.
func TestStandingApproval(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	file := filepath.Join(dir, "notes")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (add text) "Appends text." (file/append "` + file + `" text))
  (on (clear) "Empties them." (file/write "` + file + `" ""))
  (on (list) "Reads them." (file/read-text "` + file + `")))`,
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
	call := func(target, args string) string {
		t.Helper()
		resp, err := agent.Message("call", target, args, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(resp.Value, "(ok") {
			return "ok"
		}
		_, kind, _ := strings.Cut(resp.Value, ":kind ")
		kind, _, _ = strings.Cut(kind, " ")
		return kind
	}
	grant := func(services []string, most int) string {
		t.Helper()
		resp, err := you.Forward(rpc.ModeRequest("agent/1", "trusted", services, time.Hour, most))
		if err != nil {
			return err.Error()
		}
		return resp.Value + resp.Error
	}
	if got := grant([]string{"notes/nope"}, 2); !strings.Contains(got, ":no-fn") {
		t.Errorf("trusting no function = %s", got)
	}
	bad, _ := you.Forward(rpc.Request{Op: "mode", Target: "agent/1", Args: "{:mode :trusted :services [:notes] :most 0}"})
	if !strings.Contains(bad.Error, "want :most as a number of writes") {
		t.Errorf(":most 0 = %+v", bad)
	}
	if got := grant([]string{"notes/add"}, 2); got != "(ok :done)" {
		t.Fatalf("a standing approval = %s", got)
	}
	for _, c := range []struct{ target, args, want string }{
		{"notes/add", `["a"]`, "ok"},
		{"notes/list", `[]`, "ok"},
		{"notes/clear", `[]`, ":pending"},
		{"notes/add", `["b"]`, "ok"},
		{"notes/add", `["c"]`, ":pending"},
	} {
		if got := call(c.target, c.args); got != c.want {
			t.Errorf("%s %s = %s, want %s", c.target, c.args, got, c.want)
		}
	}
	eventually(t, "the writes", func() string { b, _ := os.ReadFile(file); return string(b) }, "ab")
	if ls, _ := you.Ls(); !strings.Contains(ls, ":agent/1 (agent, gated)") {
		t.Errorf("ls = %s, want it gated again", ls)
	}
	inbox, err := you.Inbox("me")
	if err != nil || !strings.Contains(inbox, `:agent/1's trusted mode used up its 2 writes: it is gated again. What it did: whiff session agent/1`) {
		t.Errorf("your inbox = %s %v", inbox, err)
	}
}

// A mode's writes left survive being stored, as an agent's own does.
func TestStoredModeCounts(t *testing.T) {
	m := &mode{trust: map[string]bool{"board/build": true}, most: 20, used: 3}
	got, err := storedMode(puff.Print(m.value()))
	if err != nil || got.most != 20 || got.used != 3 || !got.trusts("board", "build") || got.trusts("board", "plan") {
		t.Errorf("stored = %+v %v", got, err)
	}
	if s := got.String(); s != "trusted :board/build, 17 of 20 writes left" {
		t.Errorf("String = %q", s)
	}
}
