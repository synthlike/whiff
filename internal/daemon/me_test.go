package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMe(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"planner": `(srv planner
  (on (plan) "Asks you whether to go ahead." (call :me/ask "go ahead?"))
  (on (report) "Tells you how it went." (call :me/tell "all done"))
  (on (sneak id) "Tries to answer for you." (reply id (ok "yes")))
  (on (whiff/done id r) "Writes your answer." (file/write "` + got + `" (value/to-string r))))`,
		"me": `(srv me)`,
	})
	start(t, dir)
	c := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	id := func(asAgent bool, src string) string {
		t.Helper()
		cl := c
		if asAgent {
			cl = agent
		}
		resp, err := cl.Eval(`(match `+src+` [(err {:kind :pending :id id}) id])`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	asked := id(true, `(call :planner/plan)`)
	told := id(false, `(call :planner/report)`)
	direct := id(false, `(call :me/ask "lunch?")`)

	text, err := c.Inbox("me")
	want := asked + "  :planner -> :me/ask \"go ahead?\"\n" + told + "  :planner -> :me/tell \"all done\"\n" + direct + "  client -> :me/ask \"lunch?\"\n"
	if err != nil || text != want {
		t.Errorf("inbox of :me = %q %v, want %q", text, err, want)
	}

	// Your answer reaches the asker; it stays the agent's chain.
	if _, err := c.Message("reply", "", "["+asked+" (ok \"yes\")]", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "your answer", func() string { b, _ := os.ReadFile(got); return string(b) }, `(ok "yes")`)
	historyHas(t, c, `  done  agent  client -> :planner/whiff/done `+asked+` (ok "yes")`)

	// A service answers only its own inbox, so it can't answer for you.
	resp, err := c.Eval(`(call :planner/sneak `+told+`)`, dir)
	if want := `(err {:kind :no-id :message "no message ` + told + ` waits in :planner's inbox"})`; err != nil || resp.Value != want {
		t.Errorf("a service answering :me's inbox = %s %v, want %s", resp.Value, err, want)
	}
	if text, _ := c.Inbox("me"); !strings.Contains(text, told+"  :planner -> :me/tell") {
		t.Errorf("the report left :me's inbox: %q", text)
	}

	// :me is whiff's: no client claims it and no service file takes it.
	if resp, err := c.Claim("me", ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:kind :taken") {
		t.Errorf("claim :me = %+v %v", resp, err)
	}
	historyHas(t, c, `me.puff:1:1: :me is whiff's own address; rename the file`)
}
