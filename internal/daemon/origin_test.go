package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

func TestOrigin(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"relay": `(srv relay
  (on (hello) "Calls greet." (call :greet/hello))
  (on (later) "Sends greet a message in a moment." (send-after 10ms :greet/hello))
  (on (shout) "Publishes." (publish :t/x 1))
  (on (ask) "Asks todo." (call :todo/suggest "x"))
  (on (whiff/done id r) "Gets answers." (ok r)))`,
		"greet": `(srv greet
  (on (hello) "Hi." (ok :hi))
  (on :t/x (v) (ok v)))`,
		"todo": `(srv todo
  (ask (suggest text) "Suggests a todo.")
  (on (answer id) "Answers id." (reply id (ok :from-todo)))
  (on (origins) "Each waiting message's origin." (ok (vector/map (inbox) (fn (a) (match a [{:origin o} o]))))))`,
	})
	start(t, dir)
	user := dial(t, dir)
	agent := dial(t, dir)
	agent.Agent = true

	run := func(asAgent bool, src string) string {
		t.Helper()
		c := user
		if asAgent {
			c = agent
		}
		resp, err := c.Eval(src, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	inHistory := func(want string) {
		t.Helper()
		eventually(t, want, func() string {
			h, err := historyOf(user)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(h, want) {
				return want
			}
			return "not in history"
		}, want)
	}

	run(true, `(call :relay/hello)`)
	inHistory(`call  agent  :relay -> :greet/hello  (ok :hi)`)
	inHistory(`call  agent  client -> :relay/hello  (ok :hi)`)
	run(false, `(call :relay/hello)`)
	inHistory(`call  user   :relay -> :greet/hello  (ok :hi)`)

	// A send-after keeps the origin of the chain that scheduled it.
	run(true, `(call :relay/later)`)
	inHistory(`send  agent  :relay -> :greet/hello  (ok :hi)`)
	// A publish delivers with the publisher's origin.
	run(true, `(call :relay/shout)`)
	inHistory(`pub   agent  :relay -> :greet on :t/x 1  (ok 1)`)

	// An answer continues the chain of the message it answers, even when
	// the user gives it.
	id := run(true, `(match (call :relay/ask) [(err {:kind :pending :id id}) id])`)
	if got := run(false, `(call :todo/origins)`); got != `(ok [:agent])` {
		t.Errorf("origins in the inbox = %s, want (ok [:agent])", got)
	}
	if _, err := user.Message("reply", "", "["+id+" (ok 1)]", ""); err != nil {
		t.Fatal(err)
	}
	inHistory(`done  agent  client -> :relay/whiff/done ` + id + ` (ok 1)  (ok (ok 1))`)

	// An agent's answer to the user's message is the agent's: by an agent
	// client, or by a service handling an agent's message.
	id = run(false, `(match (call :relay/ask) [(err {:kind :pending :id id}) id])`)
	if _, err := agent.Message("reply", "", "["+id+" (ok 2)]", ""); err != nil {
		t.Fatal(err)
	}
	inHistory(`done  agent  client -> :relay/whiff/done ` + id + ` (ok 2)  (ok (ok 2))`)
	// todo/answer replies, which is :write: the gate holds the agent's call
	// until you accept it.
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	id = run(false, `(match (call :relay/ask) [(err {:kind :pending :id id}) id])`)
	resp, err := agent.Message("call", "todo/answer", "["+id+"]", "")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := puff.ReadValue(resp.Value)
	held, ok := pendingID(v)
	if !ok {
		t.Fatalf("agent's todo/answer = %s, want it held", resp.Value)
	}
	if _, err := user.Message("accept", "", fmt.Sprintf("[%d]", held), ""); err != nil {
		t.Fatal(err)
	}
	inHistory(`done  agent  :todo -> :relay/whiff/done ` + id + ` (ok :from-todo)  (ok (ok :from-todo))`)
	// The user's answer to the user's message stays the user's.
	id = run(false, `(match (call :relay/ask) [(err {:kind :pending :id id}) id])`)
	run(false, `(call :todo/answer `+id+`)`)
	inHistory(`done  user   :todo -> :relay/whiff/done ` + id + ` (ok :from-todo)  (ok (ok :from-todo))`)
}
