package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	greet := `(srv greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name)))
  (on (save x) :write "Saves x."
    (ok x)))`
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

	if _, err := you.Message("accept", "", "[1]", ""); err != nil {
		t.Fatal(err)
	}
	if got, isError := call("wait", map[string]any{"id": 1}); got != "[1 :a]" || isError {
		t.Errorf("wait for the accepted save = %q, error %v", got, isError)
	}
	if _, err := you.Message("call", "agent/1/tell", `["hi"]`, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := call("inbox", nil); !strings.Contains(got, `:fn :tell`) || !strings.Contains(got, `["hi"]`) {
		t.Errorf("inbox = %q, want the tell", got)
	}

	cs.Close()
	if err := <-served; err != nil {
		t.Errorf("mcpServe = %v", err)
	}
}
