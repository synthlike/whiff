package daemon

import (
	"encoding/json"
	"fmt"
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
	"time"
)

// TestHuffAgent runs an agent on huff, the real binary, built for the
// test, against a model server: told something, its turn calls
// greet/hello, which answers at once, and greet/save, a :write held for
// your review, as its session; the model hears both answers, and its last
// text is the agent's answer, in your inbox. Accepting the write gives
// the agent its result as a turn of its own, which tells you too.
// History has the calls as the agent's, and its home the transcript.
func TestHuffAgent(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	withHuff(t, dir)

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
	setEnv(t, dir, "WHIFF_LOCAL_URL", srv.URL)

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

// withHuff builds huff into dir/bin, puts it on PATH and makes dir HOME.
func withHuff(t *testing.T, dir string) {
	t.Helper()
	huff, err := builtHuff()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o700)
	if err := os.Symlink(huff, filepath.Join(bin, "huff")); err != nil {
		t.Fatal(err)
	}
	setEnv(t, dir, "PATH", bin+string(os.PathListSeparator)+getEnv(dir, "PATH"))
	setEnv(t, dir, "HOME", dir)
}

// huffDir is where builtHuff built huff, which TestMain removes.
var huffDir string

// builtHuff builds huff once for the tests that run it.
var builtHuff = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "whiff-huff-")
	if err != nil {
		return "", err
	}
	huffDir = dir
	path := filepath.Join(dir, "huff")
	if out, err := exec.Command("go", "build", "-o", path, "github.com/synthlike/whiff/cmd/huff").CombinedOutput(); err != nil {
		return "", fmt.Errorf("building huff: %v\n%s", err, out)
	}
	return path, nil
})

// TestHuffAgentStopped stops a huff turn while its model call is under
// way: huff hears the job's SIGTERM, abandons the call and keeps the
// transcript as far as it got, before the SIGKILL that would lose it, so
// the next turn goes on from it, the stopped message and all.
func TestHuffAgentStopped(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	withHuff(t, dir)
	asked := make(chan struct{}, 1)
	var mu sync.Mutex
	var second string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "second") {
			// The first turn's call hangs until huff gives it up.
			asked <- struct{}{}
			<-r.Context().Done()
			return
		}
		mu.Lock()
		second = string(b)
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Here."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"cost":0.001}}`)
	}))
	defer srv.Close()
	setEnv(t, dir, "WHIFF_LOCAL_URL", srv.URL)
	writeServices(t, dir, map[string]string{
		"scout": `(agent scout :prompt "Greet people." :model "local/fake" :can [])`,
	})
	start(t, dir)
	c := dial(t, dir)
	if resp, err := c.Eval(`(call :scout/tell "first")`, ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("tell = %+v %v", resp, err)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never called the model")
	}
	stopped := time.Now()
	if r, err := c.Eval(`(call :scout/stop)`, ""); err != nil || !strings.HasPrefix(r.Value, "(ok") {
		t.Fatalf("stop = %+v %v", r, err)
	}
	home := filepath.Join(dir, ".whiff", "agents", "scout")
	eventually(t, "the stopped turn's transcript", func() string {
		b, _ := os.ReadFile(filepath.Join(home, "huff-"+readTrim(t, filepath.Join(home, "session"))+".puff"))
		if strings.Contains(string(b), `{:role :user :text "first"}`) {
			return "kept"
		}
		return string(b)
	}, "kept")
	if took := time.Since(stopped); took > 3*time.Second {
		t.Errorf("huff took %v to stop, past the SIGKILL", took)
	}
	eventually(t, "the agent idle", func() string {
		r, _ := c.Eval(`(match (service/status :scout) [(ok {:status s}) s] [e e])`, "")
		return r.Value
	}, ":idle")
	if resp, err := c.Eval(`(call :scout/tell "second")`, ""); err != nil || !strings.HasPrefix(resp.Value, "(err {:id ") {
		t.Fatalf("tell = %+v %v", resp, err)
	}
	eventually(t, "the next turn's answer", func() string {
		text, _ := c.Inbox("me")
		if strings.Contains(text, "Here.") {
			return "answered"
		}
		return text
	}, "answered")
	mu.Lock()
	defer mu.Unlock()
	if i, j := strings.Index(second, `"first"`), strings.Index(second, "second"); i < 0 || j < i {
		t.Errorf("the next turn doesn't go on from the stopped one:\n%s", second)
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

// TestAssistantOnHuff runs services/assistant.puff as shipped, on huff,
// with its model swapped for a model server that answers what it was told
// last: told twice, it answers each in your inbox, and the second turn's
// request carries the first, one conversation.
func TestAssistantOnHuff(t *testing.T) {
	t.Parallel()
	src := shippedAssistant(t)
	dir := tempDir(t)
	withHuff(t, dir)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		var req struct {
			Messages []struct{ Content string }
		}
		json.Unmarshal(b, &req)
		text, _ := json.Marshal("Noted: " + req.Messages[len(req.Messages)-1].Content)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"cost":0.001}}`)
	}))
	defer srv.Close()
	setEnv(t, dir, "WHIFF_LOCAL_URL", srv.URL)
	writeServices(t, dir, map[string]string{
		"assistant": strings.Replace(src, assistantModel, `:model "local/fake"`, 1),
	})
	start(t, dir)
	c := dial(t, dir)
	for _, said := range []string{"the release is on Friday", "and the docs"} {
		if resp, err := c.Eval(`(call :assistant/tell "`+said+`")`, ""); err != nil || resp.Error != "" {
			t.Fatalf("tell %q = %+v %v", said, resp, err)
		}
		eventually(t, "the answer to "+said, func() string {
			text, err := c.Inbox("me")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, "Noted: "+said) {
				return "answered"
			}
			return text
		}, "answered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	for _, want := range []string{`"model":"fake"`, `Help the user keep track of what they tell you.`, `the release is on Friday`, `Noted: the release is on Friday`, `and the docs`} {
		if !strings.Contains(bodies[1], want) {
			t.Errorf("the second request has no %s:\n%s", want, bodies[1])
		}
	}
}

// TestAssistantOnOpenRouter runs services/assistant.puff as shipped, on
// its real model, when WHIFF_TEST_OPENROUTER=1 and OPENROUTER_API_KEY is
// set: told a fact, then asked it, it remembers. It costs well under a
// cent.
func TestAssistantOnOpenRouter(t *testing.T) {
	if os.Getenv("WHIFF_TEST_OPENROUTER") != "1" || os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("set WHIFF_TEST_OPENROUTER=1 and OPENROUTER_API_KEY to run it on OpenRouter")
	}
	dir := tempDir(t)
	withHuff(t, dir)
	writeServices(t, dir, map[string]string{"assistant": shippedAssistant(t)})
	start(t, dir)
	c := dial(t, dir)
	answers := 0
	for _, said := range []string{
		"Remember this: the release is on Friday. Answer only: ok.",
		"Which day is the release? Answer with the day only.",
	} {
		if resp, err := c.Eval(`(call :assistant/tell "`+said+`")`, ""); err != nil || resp.Error != "" {
			t.Fatalf("tell %q = %+v %v", said, resp, err)
		}
		answers++
		var text string
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
			var err error
			if text, err = c.Inbox("me"); err != nil {
				t.Fatal(err)
			}
			if strings.Count(text, ":assistant") >= answers {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no answer to %q: inbox %s", said, text)
			}
		}
		t.Logf("inbox after %q:\n%s", said, text)
		if answers == 2 && !strings.Contains(text, "Friday") {
			t.Errorf("it forgot the day: %s", text)
		}
	}
	if got, _ := c.Eval(`(call :assistant/usage)`, ""); got.Value != "" {
		t.Logf("usage: %s", got.Value)
	}
}

// assistantModel is the model services/assistant.puff ships with.
const assistantModel = `:model "openrouter/moonshotai/kimi-k2"`

func shippedAssistant(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "assistant.puff"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), assistantModel) {
		t.Fatalf("assistant.puff has no %s", assistantModel)
	}
	return string(src)
}
