package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
	"github.com/synthlike/whiff/internal/tools"
)

// mcpInstructions tells the agent what whiff is and how its tools behave.
const mcpInstructions = `whiff is the user's message daemon: small services written in puff, a lisp, answer messages, and whiff keeps a history of every one. You are an agent here, with an address of your own (:agent/<n>).

Each tool named address_fn calls that service function. Its arguments are puff values written as strings: 12, :key, [1 2], {:a 1}, "text" with quotes; a JSON array or object reads as a vector or map; anything else that doesn't read as a value is taken as a string. A :read function answers at once. A :write one waits for the user to review it: you get a pending id at once, and wait gives you the result once they accept or reject it. So does an ask, which waits for a person to answer.

eval runs puff code in the daemon, under the same rule: code that reaches anything :write waits for review as a whole. inbox lists the messages sent to you; answer one with eval (reply id (ok value)), which goes straight through.

The user may trust your session with some services, or free it, for a while: then the tools' descriptions say what goes straight through, and change when the mode does.`

// turnInstructions are mcpInstructions for an agent's turn, whose session
// reaches only the functions its agent's :can names.
const turnInstructions = `whiff is the user's message daemon: small services written in puff, a lisp, answer messages, and whiff keeps a history of every one. This is your turn as one of its agents, and your tools are the whiff functions you may use, nothing else.

Each tool named address_fn calls that service function. Its arguments are puff values written as strings: 12, :key, [1 2], {:a 1}, "text" with quotes; a JSON array or object reads as a vector or map; anything else that doesn't read as a value is taken as a string. A :read function answers at once. A :write one waits for the user to review it: you get a pending id at once, as you do for another agent's ask. Don't wait for it: end your turn, and its result comes to you later, as a message of its own.`

// agentIface is what an agent's address offers others: messages to it wait
// in its inbox.
const agentIface = `(ask (tell text) "Leaves the agent a message, which it reads from its inbox and answers when it next looks.")`

// mcpServe serves whiff's functions as MCP tools over t to an agent, which
// acts as :agent/<n>, until the client goes or ctx ends.
func mcpServe(ctx context.Context, dir string, t mcp.Transport) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	a := &agent{dir: dir, wd: wd, tools: map[string]string{}, token: os.Getenv("WHIFF_SESSION")}
	instructions := mcpInstructions
	if a.token != "" {
		instructions = turnInstructions
	}
	a.server = mcp.NewServer(&mcp.Implementation{Name: "whiff", Version: fmt.Sprint(rpc.Version)},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}})
	a.addFixed()
	if err := a.refresh(); err != nil {
		return err
	}
	// Services come and go: list the tools as they are now.
	a.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				a.refresh()
			}
			return next(ctx, method, req)
		}
	})
	return a.server.Run(ctx, t)
}

// agent is a whiff mcp session: its connection to the daemon, which it
// opens again if the daemon restarts, under a new address.
type agent struct {
	dir, wd string
	server  *mcp.Server

	// token is an agent turn's session token, from WHIFF_SESSION: then
	// it claims that agent's session, and offers only its :can functions,
	// with no eval or inbox.
	token string

	mu    sync.Mutex
	c     *rpc.Client
	addr  string            // its :agent/<n>, without the colon
	can   map[string]bool   // with a token: the functions it may call, address/fn
	tools map[string]string // the function tools listed, by name: their JSON, to see a change
	fns   map[string]fnTool // the same, by address/fn, to name an action's tool
	mode  string            // the session's mode as the tools describe it, printed
}

// sessionMode asks the daemon for the agent's mode.
func (a *agent) sessionMode() (tools.Mode, error) {
	v, err := a.eval("(session/mode)")
	if err != nil {
		return tools.Mode{}, err
	}
	return tools.ModeOf(v), nil
}

// modeSays is what m lets through, for the eval tool's description; "" if
// it is gated.
func modeSays(m tools.Mode) string {
	var s string
	switch {
	case m.Free:
		s = "Your session is free: everything you send, publish and evaluate goes through without review"
	case len(m.Trust) > 0:
		var names []string
		for name := range m.Trust {
			names = append(names, ":"+name)
		}
		slices.Sort(names)
		s = "Your session trusts " + strings.Join(names, " ") + ": your calls and sends to them go through without review, and so does code whose only :write is those"
	default:
		return ""
	}
	if m.Until != "" {
		s += ", until " + m.Until
	}
	return s + "."
}

// client is the agent's connection, dialled and claimed if there is none
// or the daemon has gone.
func (a *agent) client() (*rpc.Client, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.c != nil {
		select {
		case <-a.c.Done():
			a.c = nil
		default:
			return a.c, a.addr, nil
		}
	}
	c, err := dialAgent(a.dir)
	if err != nil {
		return nil, "", err
	}
	var resp rpc.Response
	if a.token != "" {
		resp, err = c.ClaimSession(a.token, agentIface)
	} else {
		resp, err = c.Claim("agent", agentIface)
	}
	var addr string
	var can map[string]bool
	if err == nil {
		v, _ := puff.ReadValue(resp.Value)
		switch v := puff.OkValue(v).(type) {
		case puff.Keyword:
			addr = string(v)
		case puff.Vector: // [:agent/<name> [fns...]], for a token
			k, _ := v.At(0).(puff.Keyword)
			fns, _ := v.At(1).(puff.Vector)
			addr, can = string(k), map[string]bool{}
			for i := range fns.Len() {
				if fn, ok := fns.At(i).(puff.Keyword); ok {
					can[string(fn)] = true
				}
			}
		}
		if addr == "" {
			err = fmt.Errorf("claiming an agent's address: %s", resp.Value)
		}
	}
	if err != nil {
		c.Close()
		return nil, "", err
	}
	a.c, a.addr, a.can = c, addr, can
	return c, addr, nil
}

// dialAgent connects to the daemon as an agent, starting it if need be.
func dialAgent(dir string) (*rpc.Client, error) {
	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		return nil, err
	}
	if err := c.Compatible("whiff mcp"); err != nil {
		c.Close()
		return nil, err
	}
	c.Agent = true
	return c, nil
}

// eval evaluates src in the daemon for the agent and returns its value,
// erring for an error or fault.
func (a *agent) eval(src string) (puff.Value, error) {
	c, _, err := a.client()
	if err != nil {
		return nil, err
	}
	return c.EvalValue(src, a.wd)
}

// fnTool is a service function as a tool, and its MCP tool.
type fnTool struct {
	tools.Fn
	tool *mcp.Tool
}

// functions are every address's functions as tools, but the agent's own
// and those only you may use, :yours, described as mode treats them.
func (a *agent) functions(mode tools.Mode) ([]fnTool, error) {
	_, self, err := a.client()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	can := a.can
	a.mu.Unlock()
	fns, err := tools.Describe(a.eval, self, can, mode)
	if err != nil {
		return nil, err
	}
	out := make([]fnTool, len(fns))
	for i, f := range fns {
		out[i] = fnTool{Fn: f, tool: &mcp.Tool{Name: f.Name, Title: f.Title, Description: f.Description, InputSchema: f.Schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: f.ReadOnly}}}
	}
	return out, nil
}

// refresh lists the functions as they are now, adding the tools that are
// new or changed and removing the ones that have gone, and describes eval
// for the session's mode.
func (a *agent) refresh() error {
	mode, err := a.sessionMode()
	if err != nil {
		return err
	}
	fns, err := a.functions(mode)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if mode.Printed != a.mode {
		a.mode = mode.Printed
		if a.token == "" {
			a.addEval(mode)
		}
	}
	now := map[string]bool{}
	a.fns = map[string]fnTool{}
	for _, f := range fns {
		now[f.tool.Name] = true
		a.fns[f.Target] = f
		js, _ := json.Marshal(f.tool)
		if a.tools[f.tool.Name] == string(js) {
			continue
		}
		a.tools[f.tool.Name] = string(js)
		a.server.AddTool(f.tool, a.callTool(f))
	}
	for name := range a.tools {
		if !now[name] {
			delete(a.tools, name)
			a.server.RemoveTools(name)
		}
	}
	return nil
}

// callTool calls f with the tool's arguments, in f's params' order up to
// the first left out.
func (a *agent) callTool(f fnTool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		vals, err := tools.Args(f.Fn, req.Params.Arguments)
		if err != nil {
			return failed("%v", err), nil
		}
		c, _, err := a.client()
		if err != nil {
			return failed("%v", err), nil
		}
		resp, err := c.Message("call", f.Target, puff.Print(vals), a.wd)
		if err != nil {
			return failed("%v", err), nil
		}
		a.noticeMode()
		return a.answer(resp.Value), nil
	}
}

// noticeMode refreshes the tools if the session's mode has changed since
// they were described, so the agent hears what goes through now.
func (a *agent) noticeMode() {
	mode, err := a.sessionMode()
	if err != nil {
		return
	}
	a.mu.Lock()
	changed := mode.Printed != a.mode
	a.mu.Unlock()
	if changed {
		a.refresh()
	}
}

// addEval adds eval, described for mode.
func (a *agent) addEval(mode tools.Mode) {
	how := "Code that reaches anything :write waits for the user's review as a whole: you get a pending id, and wait gives the result."
	if s := modeSays(mode); s != "" {
		how = s + " Other code that reaches :write waits for the user's review as a whole: you get a pending id, and wait gives the result."
		if mode.Free {
			how = s
		}
	}
	a.server.AddTool(&mcp.Tool{Name: "eval",
		Description: "Evaluates puff code in the daemon, in your working directory, and gives its value and what it printed. " + how,
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"code"},
			"properties": map[string]any{"code": map[string]any{"type": "string", "description": "puff code, such as (call :git/status)"}}}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct{ Code string }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return failed("want {\"code\": \"...\"}: %v", err), nil
			}
			c, _, err := a.client()
			if err != nil {
				return failed("%v", err), nil
			}
			resp, err := c.Eval(in.Code, a.wd)
			switch {
			case err != nil:
				return failed("%v", err), nil
			case resp.Error != "":
				return failed("%s%s", resp.Out, resp.Error), nil
			}
			a.noticeMode()
			r := a.answer(resp.Value)
			if resp.Out != "" {
				r.Content = append([]mcp.Content{&mcp.TextContent{Text: resp.Out}}, r.Content...)
			}
			return r, nil
		})
}

// addFixed adds the tools that aren't a service's and don't change with
// the mode: wait, cancel and inbox. refresh adds eval.
func (a *agent) addFixed() {
	obj := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	a.server.AddTool(&mcp.Tool{Name: "wait",
		Description: "Waits for the result of a pending message, eval or job, by its id, up to seconds (30 by default); if it is still pending then, it says so, and you can wait again later. Ids last across reconnects.",
		InputSchema: obj(map[string]any{
			"id":      map[string]any{"type": "integer", "description": "the pending id"},
			"seconds": map[string]any{"type": "integer", "description": "how long to wait, at most 600"},
		}, "id"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct{ ID, Seconds int64 }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return failed("want {\"id\": n}: %v", err), nil
			}
			wait := 30 * time.Second
			if in.Seconds > 0 {
				wait = min(time.Duration(in.Seconds)*time.Second, 10*time.Minute)
			}
			return a.wait(ctx, in.ID, wait), nil
		})
	a.server.AddTool(&mcp.Tool{Name: "cancel",
		Description: "Cancels a pending message or job of yours, by its id: one waiting for review or a reply is dropped, and a running job is killed. Its result says how it ended: :outcome :not-started, or :stopped, as what it had done stays done.",
		InputSchema: obj(map[string]any{
			"id": map[string]any{"type": "integer", "description": "the pending id"},
		}, "id")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct{ ID int64 }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return failed("want {\"id\": n}: %v", err), nil
			}
			c, _, err := a.client()
			if err != nil {
				return failed("%v", err), nil
			}
			resp, err := c.Message("cancel-task", "", fmt.Sprintf("[%d]", in.ID), a.wd)
			if err != nil {
				return failed("%v", err), nil
			}
			return a.answer(resp.Value), nil
		})
	if a.token != "" {
		return // an agent's turn answers its service, not an inbox
	}
	a.server.AddTool(&mcp.Tool{Name: "inbox",
		Description: "Lists the messages sent to you that wait for your answer, oldest first, each {:id :fn :args :from :origin :cwd :at}. Answer one with eval (reply id (ok value)).",
		InputSchema: obj(map[string]any{}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			v, err := a.eval("(inbox)")
			if err != nil {
				return failed("%v", err), nil
			}
			if vec, ok := v.(puff.Vector); ok && vec.Len() == 0 {
				return toolText("no messages"), nil
			}
			return toolText(puff.Print(v)), nil
		})
}

// wait waits up to d for the answer to id, on a connection of its own so
// the agent's other tools don't wait behind it.
func (a *agent) wait(ctx context.Context, id int64, d time.Duration) *mcp.CallToolResult {
	c, err := dialAgent(a.dir)
	if err != nil {
		return failed("%v", err)
	}
	defer c.Close()
	type answer struct {
		resp rpc.Response
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		resp, err := c.Message("wait", "", fmt.Sprintf("[%d]", id), "")
		done <- answer{resp, err}
	}()
	select {
	case ans := <-done:
		if ans.err != nil {
			return failed("%v", ans.err)
		}
		return a.answer(ans.resp.Value)
	case <-time.After(d):
		return toolText(fmt.Sprintf("%d is still pending: wait again later", id))
	case <-ctx.Done():
		return failed("cancelled")
	}
}

// resultOf is a printed Result as a tool's result, as tools.Result
// reads it.
func resultOf(printed string) *mcp.CallToolResult {
	text, isErr := tools.Result(printed, "call wait with this id for the result")
	r := toolText(text)
	r.IsError = isErr
	return r
}

// answer is resultOf(printed), and, if its value offers actions as a view's
// items do, each as the tool call that sends it: what the agent can do from
// here.
func (a *agent) answer(printed string) *mcp.CallToolResult {
	r := resultOf(printed)
	if r.IsError {
		return r
	}
	a.mu.Lock()
	fns := map[string]tools.Fn{}
	for k, f := range a.fns {
		fns[k] = f.Fn
	}
	a.mu.Unlock()
	if lines := tools.Offers(printed, fns); len(lines) > 0 {
		r.Content = append(r.Content, &mcp.TextContent{Text: "What you can do from here, each as a tool call:\n" + strings.Join(lines, "\n")})
	}
	return r
}

func toolText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func failed(format string, args ...any) *mcp.CallToolResult {
	r := toolText(fmt.Sprintf(format, args...))
	r.IsError = true
	return r
}

// mcpRun is whiff mcp: mcpServe on stdin and stdout.
func mcpRun(dir string, stderr io.Writer) int {
	if err := mcpServe(context.Background(), dir, &mcp.StdioTransport{}); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}
