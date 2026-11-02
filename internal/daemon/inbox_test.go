package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInbox(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"todo": `(service todo
  (ask (suggest text) "Suggests a todo.")
  (ask (quick) "Waits a moment." :expires 50ms)
  (def (timeless items) (vector/map items (fn (a) (map/delete a :at))))
  (on (items) "The inbox, without times." (ok (timeless (inbox))))
  (on (suggested) "Only the suggestions." (ok (timeless (inbox :suggest))))
  (on (recent) "Whether each message came in the last minute."
    (ok (vector/map (inbox) (fn (a) (match a [{:at t} (< (time/since t) 1m)])))))
  (on (answer id v) "Answers id." (reply id (ok v))))`,
		"asker": `(service asker
  (on (ask text) "Asks todo." (call :todo/suggest text))
  (on (whiff/done id r) "Writes the answer."
    (match r
      [(ok v) (file/write "` + got + `" v)]
      [(err e) (file/write "` + got + `" "err")])))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// pending is the [kind id] of an err, or the value of an ok.
	pending := func(call string) string {
		return `(match ` + call + ` [(err {:kind :pending :id id}) [:pending id]] [(err {:kind k}) k] [(ok v) v])`
	}
	tests := []struct{ src, want string }{
		{pending(`(call :todo/suggest "docs")`), `[:pending 1]`},
		{pending(`(call :asker/ask "tests")`), `[:pending 2]`},
		{pending(`(call :todo/quick)`), `[:pending 3]`},
		{pending(`(call :todo/suggest)`), `:bad-call`},
		{`(call :todo/items)`, `(ok [{:args ["docs"] :cwd "/tmp/w" :fn :suggest :id 1 :origin :user} {:args ["tests"] :cwd "/tmp/w" :fn :suggest :from :asker :id 2 :origin :user} {:args [] :cwd "/tmp/w" :fn :quick :id 3 :origin :user}])`},
		{`(call :todo/suggested)`, `(ok [{:args ["docs"] :cwd "/tmp/w" :fn :suggest :id 1 :origin :user} {:args ["tests"] :cwd "/tmp/w" :fn :suggest :from :asker :id 2 :origin :user}])`},
		{`(call :todo/recent)`, `(ok [#t #t #t])`},
		{`(call :todo/answer 2 "done tests")`, `(ok :done)`},
		{`(call :todo/answer 2 "again")`, `(err {:kind :no-id :message "no message 2 waits in :todo's inbox"})`},
	}
	for _, tt := range tests {
		resp, err := c.Eval(tt.src, "/tmp/w")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if resp.Value != tt.want || resp.Error != "" {
			t.Errorf("%s = %s %s, want %s", tt.src, resp.Value, resp.Error, tt.want)
		}
	}

	// asker's whiff/done handler gets the answer.
	eventually(t, "the answer", func() string { b, _ := os.ReadFile(got); return string(b) }, "done tests")

	text, err := c.Inbox("todo")
	if want := "1  client -> :todo/suggest \"docs\"\n"; err != nil || !strings.HasPrefix(text, want) {
		t.Errorf("inbox = %q %v, want it to start with %q", text, err, want)
	}

	// A client answers any inbox; nobody handles the answer, so it is in history.
	resp, err := c.Message("reply", "", `[1 (ok "yes")]`, "")
	if err != nil || resp.Value != "(ok :done)" {
		t.Errorf("reply 1 = %+v %v, want (ok :done)", resp, err)
	}
	if _, err := c.Message("reply", "", `[1 5]`, ""); err == nil || err.Error() != "want a Result, (ok v) or (err e), got 5" {
		t.Errorf("reply 1 5: %v", err)
	}
	history := func() string {
		h, err := historyOf(c)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, want := range []string{
		`done  user   client -> client 1 (ok "yes")`,
		`done  user   :todo -> :asker/whiff/done 2 (ok "done tests")  (ok :done)`,
		`done  user   :todo -> client 3 (err {:kind :expired :message ":todo/quick waited longer than 50ms" :outcome :not-started})`,
	} {
		eventually(t, want, func() string {
			if strings.Contains(history(), want) {
				return want
			}
			return "not in history"
		}, want)
	}
	if text, _ := c.Inbox("todo"); text != "" {
		t.Errorf("inbox after the replies = %q, want it empty", text)
	}

	resp, err = c.Eval("(inbox)", "")
	if want := "eval:1:1: inbox: only a service has an inbox"; err != nil || !resp.Fault || resp.Error != want {
		t.Errorf("(inbox) from a client = %+v %v, want fault %q", resp, err, want)
	}
}

func TestAskDef(t *testing.T) {
	dir := tempDir(t)
	tests := []struct{ name, src, want string }{
		{"ok", `(service ok (ask (f x) :write "Doc." :expires 1d))`, ""},
		{"nodoc", `(service nodoc (ask (f x)))`, ":1:16: ask wants (fn params...) and a doc string"},
		{"body", `(service body (ask (f) "Doc." (ok 1)))`, ":1:31: ask takes :expires d after its doc string, got (ok 1)"},
		{"expires", `(service expires (ask (f) "Doc." :expires 1))`, ":1:43: want a positive :expires duration, got 1"},
		{"clash", `(service clash (ask (f) "Doc.") (on (f) "Doc." (ok 1)))`, ":1:38: f is already defined at"},
	}
	d := &daemon{ctx: context.Background()}
	for _, tt := range tests {
		path := filepath.Join(dir, tt.name+".puff")
		if err := os.WriteFile(path, []byte(tt.src), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := d.loadService(path)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// eventually waits until get returns want.
func eventually(t *testing.T, what string, get func() string, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got = get(); got == want {
			return
		}
	}
	t.Errorf("%s: got %q, want %q", what, got, want)
}
