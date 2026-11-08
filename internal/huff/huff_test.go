package huff

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// A call goes out as Chat Completions: the system prompt first, an
// assistant's tool calls with null content, a tool's answer naming its
// call, the tools as functions; and its answer comes back as a Reply.
func TestChat(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"c2","type":"function","function":{"name":"todo_add","arguments":"{\"text\":\"milk\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"completion_tokens":14,"cost":0.0021}}`)
	}))
	defer srv.Close()

	c := &Chat{BaseURL: srv.URL + "/v1", Key: "k"}
	reply, err := c.Complete(context.Background(), Request{
		Model:  "moonshotai/kimi-k2",
		System: "You keep a list.",
		Messages: []Message{
			{Role: "user", Text: "what's on it?"},
			{Role: "assistant", Calls: []Call{{ID: "c1", Fn: "todo_list", Args: "{}"}}},
			{Role: "tool", ID: "c1", Result: "[]"},
			{Role: "user", Text: "add milk"},
		},
		Tools: []Tool{{Name: "todo_add", Description: "Adds an item.", Params: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer k" {
		t.Errorf("auth = %q", auth)
	}
	want := `{"messages":[{"content":"You keep a list.","role":"system"},{"content":"what's on it?","role":"user"},` +
		`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"todo_list"},"id":"c1","type":"function"}]},` +
		`{"content":"[]","role":"tool","tool_call_id":"c1"},{"content":"add milk","role":"user"}],` +
		`"model":"moonshotai/kimi-k2","tools":[{"function":{"description":"Adds an item.","name":"todo_add","parameters":{"type":"object"}},"type":"function"}]}`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Errorf("request:\n%s\nwant\n%s", b, want)
	}
	if reply.Stop != "tools" || len(reply.Message.Calls) != 1 || reply.Message.Calls[0] != (Call{ID: "c2", Fn: "todo_add", Args: `{"text":"milk"}`}) {
		t.Errorf("reply = %+v", reply)
	}
	if reply.Usage != (Usage{Input: 120, Output: 14, Cost: 0.0021}) {
		t.Errorf("usage = %+v", reply.Usage)
	}
}

// A 429 or a 5xx is tried again; a 4xx isn't, and says the server's
// message; an error in a 200, as OpenRouter's from a model's provider, is
// tried again too.
func TestChatErrors(t *testing.T) {
	Backoff = time.Millisecond
	defer func() { Backoff = time.Second }()
	var n atomic.Int32
	answers := []func(w http.ResponseWriter){}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answers[int(n.Add(1))-1](w)
	}))
	defer srv.Close()
	limited := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down","code":429}}`)
	}
	upstream := func(w http.ResponseWriter) { io.WriteString(w, `{"error":{"message":"provider down","code":502}}`) }
	ok := func(w http.ResponseWriter) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}
	bad := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"no such model","code":"model_not_found"}}`)
	}
	c := &Chat{BaseURL: srv.URL}
	call := func() (Reply, error) {
		return c.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Text: "hi"}}})
	}

	credits := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":{"message":"would exceed your available credits given your current in-flight requests","code":402}}`)
	}
	answers, n = []func(http.ResponseWriter){limited, upstream, credits, ok}, atomic.Int32{}
	if r, err := call(); err != nil || r.Message.Text != "hi" || r.Stop != "end" || n.Load() != 4 {
		t.Errorf("after a 429, an upstream error and a 402 = %+v %v, %d tries", r, err, n.Load())
	}
	answers, n = []func(http.ResponseWriter){bad, ok}, atomic.Int32{}
	if _, err := call(); err == nil || !strings.Contains(err.Error(), "400 no such model") || n.Load() != 1 {
		t.Errorf("a 400 = %v, %d tries", err, n.Load())
	}
	answers, n = []func(http.ResponseWriter){limited, limited, limited, limited, limited, ok}, atomic.Int32{}
	if _, err := call(); err == nil || !strings.Contains(err.Error(), "429 slow down") || n.Load() != 5 {
		t.Errorf("past its retries = %v, %d tries", err, n.Load())
	}
	garbled := func(w http.ResponseWriter) { io.WriteString(w, `{"choices":[]}`) }
	cut := func(w http.ResponseWriter) { io.WriteString(w, `{"choices":[{"mess`) }
	answers, n = []func(http.ResponseWriter){garbled, cut, ok}, atomic.Int32{}
	if r, err := call(); err != nil || r.Message.Text != "hi" || n.Load() != 3 {
		t.Errorf("after no choices and a cut answer = %+v %v, %d tries", r, err, n.Load())
	}
}

// A model's name picks its provider, and says what's missing.
func TestOpen(t *testing.T) {
	env := map[string]string{"OPENROUTER_API_KEY": "or"}
	getenv := func(k string) string { return env[k] }
	p, model, err := Open("openrouter/moonshotai/kimi-k2", getenv)
	if c, _ := p.(*Chat); err != nil || model != "moonshotai/kimi-k2" || c.BaseURL != "https://openrouter.ai/api/v1" || c.Key != "or" {
		t.Errorf("openrouter = %+v %q %v", p, model, err)
	}
	if p, model, err := Open("local/qwen3", getenv); err != nil || model != "qwen3" || p.(*Chat).BaseURL != "http://localhost:11434/v1" || p.(*Chat).Key != "" {
		t.Errorf("local = %+v %q %v", p, model, err)
	}
	for name, want := range map[string]string{
		"openai/gpt-5": "OPENAI_API_KEY",
		"kimi-k2":      "provider/model",
		"anthropic/x":  "no provider",
		"openrouter/":  "provider/model",
	} {
		if _, _, err := Open(name, getenv); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %v, want %s", name, err, want)
		}
	}
}

// A transcript reads back as it was printed, keeping only keys that say
// something.
func TestTranscript(t *testing.T) {
	ms := []Message{
		{Role: "user", Text: "add milk"},
		{Role: "assistant", Calls: []Call{{ID: "c1", Fn: "todo_add", Args: `{"text":"milk"}`}}},
		{Role: "tool", ID: "c1", Result: "waiting for your review as task 7", IsError: false},
		{Role: "tool", ID: "c2", Result: "no such tool", IsError: true},
		{Role: "assistant", Text: "Added, once you accept it."},
	}
	printed := puff.Print(ToPuff(ms))
	want := `[{:role :user :text "add milk"} {:calls [{:args "{\"text\":\"milk\"}" :fn "todo_add" :id "c1"}] :role :assistant} ` +
		`{:id "c1" :result "waiting for your review as task 7" :role :tool} {:error #t :id "c2" :result "no such tool" :role :tool} ` +
		`{:role :assistant :text "Added, once you accept it."}]`
	if printed != want {
		t.Errorf("printed:\n%s\nwant\n%s", printed, want)
	}
	v, err := puff.ReadValue(printed)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromPuff(v)
	if err != nil || puff.Print(ToPuff(back)) != printed {
		t.Errorf("read back = %+v %v", back, err)
	}
	for _, src := range []string{`{:role :user}`, `[{:role :system :text "x"}]`, `[{:role :tool :result "x"}]`} {
		v, _ := puff.ReadValue(src)
		if _, err := FromPuff(v); err == nil {
			t.Errorf("%s read as a transcript", src)
		}
	}
	if got := puff.Print(UsagePuff(Usage{Input: 3, Output: 1, Cost: 0.5})); got != "{:cost 0.5 :input 3 :output 1}" {
		t.Errorf("usage = %s", got)
	}
}
