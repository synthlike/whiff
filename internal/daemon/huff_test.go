package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestHuffAgent runs an agent on huff, the real binary, built for the
// test, against a model server: told something, its turn calls
// greet/hello, which answers at once, and greet/save, a :write held for
// your review, as its session; the model hears both answers, and its last
// text is the agent's answer, in your inbox. Accepting the write gives
// the agent its result as a turn of its own, which tells you too.
// History has the calls as the agent's, and its home the transcript.
func TestHuffAgent(t *testing.T) {
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "huff"), "github.com/synthlike/whiff/cmd/huff")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building huff: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)

	// The model: asked by the user, it calls both tools; given their
	// answers, it says them.
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		var req struct {
			Model    string
			Messages []struct {
				Role    string
				Content *string
			}
			Tools []struct{ Function struct{ Name string } }
		}
		json.Unmarshal(b, &req)
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(*last.Content, "has ended") {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Saved it."},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":3,"cost":0.002}}`)
			return
		}
		if last.Role == "user" {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"greet_hello","arguments":"{\"name\":\"\\\"you\\\"\"}"}},
				{"id":"c2","type":"function","function":{"name":"greet_save","arguments":"{\"x\":\"1\"}"}}]},
				"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"cost":0.001}}`)
			return
		}
		var said []string
		for _, m := range req.Messages {
			if m.Role == "tool" && m.Content != nil {
				said = append(said, *m.Content)
			}
		}
		text, _ := json.Marshal("Done: " + strings.Join(said, " | "))
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":80,"completion_tokens":12,"cost":0.002}}`)
	}))
	defer srv.Close()
	t.Setenv("WHIFF_LOCAL_URL", srv.URL)

	writeServices(t, dir, map[string]string{
		"scout": `(agent scout :prompt "Greet people." :model "local/fake" :can [:greet/hello :greet/save])`,
		"greet": `(service greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name)))
  (on (save x) :write "Saves x." (ok x)))`,
	})
	start(t, dir)
	c := dial(t, dir)
	// A reviewer, so the write is held rather than refused.
	reviewer := dial(t, dir)
	if _, err := reviewer.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Eval(`(call :scout/tell "hi")`, ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("tell = %+v %v", resp, err)
	}
	inbox := func() string {
		text, err := c.Inbox("me")
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	held := regexp.MustCompile(`Done: hello, you \| pending (\d+): [^"]*don't wait for it`)
	var id string
	eventually(t, "the turn's answer", func() string {
		if m := held.FindStringSubmatch(inbox()); m != nil {
			id = m[1]
			return "answered"
		}
		return inbox()
	}, "answered")
	// Accepting runs the held call, and answers what it did.
	if r, err := c.Message("accept", "", "["+id+"]", ""); err != nil || r.Value != "(ok 1)" {
		t.Fatalf("accept %s = %+v %v", id, r, err)
	}
	eventually(t, "the result's turn", func() string {
		if strings.Contains(inbox(), "Saved it.") {
			return "saved"
		}
		return inbox()
	}, "saved")

	mu.Lock()
	first := bodies[0]
	mu.Unlock()
	for _, want := range []string{`"model":"fake"`, `Greet people.`, `Your tools are whiff functions`, `"name":"greet_hello"`, `"name":"greet_save"`, `"name":"scout_later"`} {
		if !strings.Contains(first, want) {
			t.Errorf("the first request has no %s:\n%s", want, first)
		}
	}
	historyHas(t, c, `:agent/scout -> :greet/hello "you"  (ok "hello, you")`)
	historyHas(t, c, `:agent/scout -> :greet/save 1  (err {:id`)
	b, err := os.ReadFile(filepath.Join(dir, ".whiff", "agents", "scout", "huff-"+readTrim(t, filepath.Join(dir, ".whiff", "agents", "scout", "session"))+".puff"))
	if err != nil || !strings.Contains(string(b), `{:role :user :text "hi"}`) || !strings.Contains(string(b), `:fn "greet_save"`) {
		t.Errorf("transcript = %s %v", b, err)
	}
	// What its turn cost is the agent's, as a claude turn's is.
	if got, _ := c.Eval(`(match (call :scout/usage) [(ok {:spent s :turns n}) [s n]] [e e])`, ""); got.Value != "[0.005 2]" {
		t.Errorf("usage = %s %s", got.Value, got.Error)
	}
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}
