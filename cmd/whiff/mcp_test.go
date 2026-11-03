package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

func TestMCP(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	services := filepath.Join(dir, "services")
	os.Mkdir(services, 0o700)
	greet := `(service greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name)))
  (on (save x) :write "Saves x."
    (ok x))
  (on (menu) "What to do next."
    (ok (view/items [{:label "Say hi" :action [:greet/hello "you"]}
                     {:label "Keep it" :action [:greet/save] :ask "What?"}
                     {:label "Answer" :action [:me/answer 1]}]))))`
	if err := os.WriteFile(filepath.Join(services, "greet.puff"), []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHIFF_TEST_DIR", dir)
	t.Cleanup(func() { run([]string{"stop"}, dir, services, new(strings.Builder), new(strings.Builder)) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	served := make(chan error, 1)
	go func() { served <- mcpServe(ctx, dir, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"greet_hello", "greet_save", "me_ask", "eval", "wait", "inbox"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v have no %s", names, want)
		}
	}
	if slices.Contains(names, "agent_1_tell") {
		t.Errorf("tools %v have the agent's own agent_1_tell", names)
	}

	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range r.Content {
			b.WriteString(c.(*mcp.TextContent).Text)
		}
		return b.String(), r.IsError
	}

	// You review agents' writes, following :review/pending.
	you, err := rpc.Dial(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer you.Close()
	if _, err := you.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    map[string]any
		want    string
		isError bool
	}{
		{"greet_hello", map[string]any{"name": "ann"}, "hello, ann", false},
		{"greet_hello", map[string]any{"name": "12"}, "", true}, // 12 is an int: string/concat faults
		{"greet_hello", nil, `{:kind :bad-call`, true},
		{"greet_save", map[string]any{"x": "[1 :a]"}, "pending 1: :greet/save waits for your review; call wait with this id for the result", false},
		{"eval", map[string]any{"code": `(io/println "hi") (+ 1 2)`}, "hi\n3", false},
		{"eval", map[string]any{"code": `(file/write "x" "y")`}, "pending 2: the agent's eval waits for your review; call wait with this id for the result", false},
		{"eval", map[string]any{"code": `(+ 1`}, "eval:1:1: no closing ')'", true},
		{"wait", map[string]any{"id": 2, "seconds": 1}, "2 is still pending: wait again later", false},
		{"inbox", nil, "no messages", false},
	}
	for _, tt := range tests {
		got, isError := call(tt.name, tt.args)
		if isError != tt.isError || tt.want != "" && !strings.HasPrefix(got, tt.want) {
			t.Errorf("%s %v = %q, error %v; want %q, error %v", tt.name, tt.args, got, isError, tt.want, tt.isError)
		}
	}

	// A view's actions come as tool calls, saying which wait for review;
	// one only you may use isn't offered.
	next := "What you can do from here, each as a tool call:\n" +
		`- Say hi: greet_hello {"name": "\"you\""}` + "\n" +
		`- Keep it: greet_save {"x": <your answer to "What?">}, which waits for the user's review`
	if got, _ := call("greet_menu", nil); !strings.HasSuffix(got, next) {
		t.Errorf("greet_menu = %q, want it to end with\n%s", got, next)
	}

	if _, err := you.Message("accept", "", "[1]", ""); err != nil {
		t.Fatal(err)
	}
	if got, isError := call("wait", map[string]any{"id": 1}); got != "[1 :a]" || isError {
		t.Errorf("wait for the accepted save = %q, error %v", got, isError)
	}
	told, err := you.Message("call", "agent/1/tell", `["hi"]`, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := call("inbox", nil); !strings.Contains(got, `:fn :tell`) || !strings.Contains(got, `["hi"]`) {
		t.Errorf("inbox = %q, want the tell", got)
	}
	// Its answer goes straight through, without a review.
	v, _ := puff.ReadValue(told.Value)
	n := puff.Print(field(v.(puff.Tag).At(0), "id"))
	if got, isError := call("eval", map[string]any{"code": "(reply " + n + ` (ok "hello"))`}); got != ":done" || isError {
		t.Errorf("reply = %q, error %v", got, isError)
	}
	if got, _ := call("inbox", nil); got != "no messages" {
		t.Errorf("inbox after the reply = %q", got)
	}

	// Once you trust it with :greet, the next call notices, and the tools
	// say what goes through.
	if r, err := you.Mode("agent/1", "trusted", []string{"greet"}, 0); err != nil || r.Value != "(ok :done)" {
		t.Fatalf("trust = %+v %v", r, err)
	}
	call("greet_hello", map[string]any{"name": "ann"})
	list, err = cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"greet_save": "Your session's mode lets it through without review: it runs at once.",
		"eval":       "Your session trusts :greet: your calls and sends to them go through without review",
	} {
		i := slices.IndexFunc(list.Tools, func(tool *mcp.Tool) bool { return tool.Name == name })
		if i < 0 || !strings.Contains(list.Tools[i].Description, want) {
			t.Errorf("%s's description doesn't say %q", name, want)
		}
	}
	// What only you may use isn't a tool: reviewing, and your inbox.
	for name, want := range map[string]bool{"me_ask": true, "me_inbox": false, "me_answer": false, "review_accept": false, "review_pending": false} {
		if got := slices.ContainsFunc(list.Tools, func(tool *mcp.Tool) bool { return tool.Name == name }); got != want {
			t.Errorf("tool %s offered = %v, want %v", name, got, want)
		}
	}
	if got, isError := call("greet_save", map[string]any{"x": "2"}); got != "2" || isError {
		t.Errorf("trusted save = %q, error %v", got, isError)
	}

	cs.Close()
	if err := <-served; err != nil {
		t.Errorf("mcpServe = %v", err)
	}
}

// TestMCPTurn serves whiff mcp as an agent's turn runs it, with the
// WHIFF_SESSION its job got: it acts as the agent's session, lists only
// the agent's :can functions with wait and cancel, and has no eval or
// inbox.
func TestMCPTurn(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "whiff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	services, bin := filepath.Join(dir, "services"), filepath.Join(dir, "bin")
	os.Mkdir(services, 0o700)
	os.Mkdir(bin, 0o700)
	files := map[string]string{
		filepath.Join(services, "scout.puff"): `(agent scout :prompt "Greet people." :can [:greet/hello])`,
		filepath.Join(services, "greet.puff"): `(service greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name)))
  (on (save x) :write "Saves x." (ok x)))`,
		// The turn leaves its token and holds until the test is done.
		filepath.Join(bin, "claude"): "#!/bin/sh\necho \"$WHIFF_SESSION\" > token\nwhile [ ! -e go ]; do sleep 0.02; done\necho done\n",
	}
	for path, src := range files {
		if err := os.WriteFile(path, []byte(src), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("WHIFF_TEST_DIR", dir)
	t.Cleanup(func() { run([]string{"stop"}, dir, services, new(strings.Builder), new(strings.Builder)) })
	home := filepath.Join(dir, ".whiff", "agents", "scout")
	t.Cleanup(func() { os.WriteFile(filepath.Join(home, "go"), nil, 0o600) })

	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var token string
	for deadline := time.Now().Add(5 * time.Second); token == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if resp, err := c.Eval(`(call :scout/ask "hi")`, ""); err != nil || resp.Error != "" {
			continue // scout may not have loaded yet
		}
		for wait := time.Now().Add(2 * time.Second); token == "" && time.Now().Before(wait); time.Sleep(20 * time.Millisecond) {
			b, _ := os.ReadFile(filepath.Join(home, "token"))
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		t.Fatal("the turn left no token")
	}
	t.Setenv("WHIFF_SESSION", token)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	go mcpServe(ctx, dir, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"cancel", "greet_hello", "wait"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "greet_hello", Arguments: map[string]any{"name": `"you"`}})
	if err != nil || r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, "hello, you") {
		t.Errorf("greet_hello = %+v %v", r, err)
	}
	// History has the call as the agent's session's.
	hist, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(hist, func(e rpc.Entry) bool { return e.From == "agent/scout" && e.To == "greet/hello" }) {
		t.Errorf("history has no call from :agent/scout to :greet/hello: %+v", hist)
	}
}
