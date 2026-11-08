package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// model is a server that answers each request with how many messages it
// was sent and the last one's text.
func model(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		last := req.Messages[len(req.Messages)-1]
		text, _ := json.Marshal(strings.TrimSpace(last.Content) + " (" + string(rune('0'+len(req.Messages))) + " messages)")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+string(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"cost":0.25}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runHuff(t *testing.T, env map[string]string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errs strings.Builder
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errs, func(k string) string { return env[k] })
	return out.String(), errs.String(), code
}

// On its own, huff answers a prompt from its arguments or stdin, and with
// -t goes on from a transcript and keeps the turn in it.
func TestHuff(t *testing.T) {
	env := map[string]string{"WHIFF_LOCAL_URL": model(t)}
	if out, errs, code := runHuff(t, env, "", "-m", "local/x", "-s", "Be brief.", "hello", "there"); code != 0 || out != "hello there (2 messages)\n" {
		t.Errorf("args: %q %q %d", out, errs, code)
	}
	if out, _, code := runHuff(t, env, "from stdin\n", "-m", "local/x"); code != 0 || out != "from stdin (1 messages)\n" {
		t.Errorf("stdin: %q %d", out, code)
	}
	tr := filepath.Join(t.TempDir(), "t.puff")
	runHuff(t, env, "", "-m", "local/x", "-t", tr, "one")
	if out, _, _ := runHuff(t, env, "", "-m", "local/x", "-t", tr, "two"); out != "two (3 messages)\n" {
		t.Errorf("going on: %q", out)
	}
	b, _ := os.ReadFile(tr)
	if !strings.Contains(string(b), `{:role :user :text "two"}`) || strings.Count(string(b), ":role :assistant") != 2 {
		t.Errorf("transcript: %s", b)
	}
	// $HUFF_MODEL names the model unless -m does.
	env["HUFF_MODEL"] = "local/x"
	if out, _, code := runHuff(t, env, "", "hi"); code != 0 || out != "hi (1 messages)\n" {
		t.Errorf("HUFF_MODEL: %q %d", out, code)
	}
}

// -json prints claude -p's stream-json: an init line, each step, and how
// it ended, with its cost.
func TestHuffJSON(t *testing.T) {
	env := map[string]string{"WHIFF_LOCAL_URL": model(t)}
	out, _, code := runHuff(t, env, "", "-json", "-m", "local/x", "hi")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[0], `"subtype":"init"`) || !strings.Contains(lines[1], `"text":"hi (1 messages)"`) {
		t.Fatalf("json: %d\n%s", code, out)
	}
	var res struct {
		Type, Subtype, Result string
		IsError               bool    `json:"is_error"`
		Cost                  float64 `json:"total_cost_usd"`
		Turns                 int     `json:"num_turns"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &res); err != nil || res.Type != "result" || res.Subtype != "success" || res.IsError || res.Result != "hi (1 messages)" || res.Cost != 0.25 || res.Turns != 1 {
		t.Errorf("result: %+v %v", res, err)
	}
}

// What's missing says so.
func TestHuffErrors(t *testing.T) {
	for _, tt := range []struct {
		env  map[string]string
		args []string
		want string
	}{
		{map[string]string{}, []string{"hi"}, "name a model"},
		{map[string]string{}, []string{"-m", "openrouter/moonshotai/kimi-k2", "hi"}, "OPENROUTER_API_KEY"},
		{map[string]string{}, []string{"-m", "local/x"}, "give a prompt"},
		{map[string]string{"WHIFF_LOCAL_URL": "http://127.0.0.1:1"}, []string{"-m", "local/x", "hi"}, "connection refused"},
	} {
		if _, errs, code := runHuff(t, tt.env, "", tt.args...); code == 0 || !strings.Contains(errs, tt.want) {
			t.Errorf("%v: %q %d, want %s", tt.args, errs, code, tt.want)
		}
	}
}

// With -tools, the model works on files in -dir, and Bash runs only what
// -allow names: what it refused is in the result, as claude -p's.
func TestHuffTools(t *testing.T) {
	calls := []string{
		`{"id":"a","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"notes.txt\",\"content\":\"hi\\n\"}"}}`,
		`{"id":"b","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"rm notes.txt\"}"}}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Tools []struct{ Function struct{ Name string } } }
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) != 2 {
			t.Errorf("tools offered: %+v", req.Tools)
		}
		if n < len(calls) {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[`+calls[n]+`]},"finish_reason":"tool_calls"}]}`)
		} else {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
		n++
	}))
	defer srv.Close()
	dir := t.TempDir()
	out, errs, code := runHuff(t, map[string]string{"WHIFF_LOCAL_URL": srv.URL}, "", "-json", "-m", "local/x",
		"-dir", dir, "-tools", "Write,Bash", "-allow", "go test ./...", "write notes")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || !strings.Contains(lines[len(lines)-1], `"permission_denials":[{"tool_input":{"command":"rm notes.txt"},"tool_name":"Bash"}]`) {
		t.Errorf("result: %d %s\n%s", code, errs, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "notes.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("notes.txt: %q %v", b, err)
	}
	if _, errs, code := runHuff(t, nil, "", "-m", "local/x", "-tools", "Read,Shell", "x"); code != 1 || !strings.Contains(errs, "there is no tool Shell") {
		t.Errorf("an unknown tool: %d %s", code, errs)
	}
}
