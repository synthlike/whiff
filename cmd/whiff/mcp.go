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
)

// mcpInstructions tells the agent what whiff is and how its tools behave.
const mcpInstructions = `whiff is the user's message daemon: small services written in puff, a lisp, answer messages, and whiff keeps a history of every one. You are an agent here, with an address of your own (:agent/<n>).

Each tool named address_fn calls that service function. Its arguments are puff values written as strings: 12, :key, [1 2], {:a 1}, "text" with quotes; anything that doesn't read as a value is taken as a string. A :read function answers at once. A :write one waits for the user to review it: you get a pending id at once, and wait gives you the result once they accept or reject it. So does an ask, which waits for a person to answer.

eval runs puff code in the daemon, under the same rule: code that reaches anything :write waits for review as a whole. inbox lists the messages sent to you; answer one with eval (reply id (ok value)), which goes straight through.

The user may trust your session with some services, or free it, for a while: then the tools' descriptions say what goes straight through, and change when the mode does.`

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
	a := &agent{dir: dir, wd: wd, tools: map[string]string{}}
	a.server = mcp.NewServer(&mcp.Implementation{Name: "whiff", Version: fmt.Sprint(rpc.Version)},
		&mcp.ServerOptions{Instructions: mcpInstructions, Capabilities: &mcp.ServerCapabilities{}})
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

	mu    sync.Mutex
	c     *rpc.Client
	addr  string            // its :agent/<n>, without the colon
	tools map[string]string // the function tools listed, by name: their JSON, to see a change
	mode  string            // the session's mode as the tools describe it, printed
}

// sessionMode is the agent's mode, as (mode) gives it: what it lets
// through without review.
type sessionMode struct {
	printed string
	free    bool
	trust   map[string]bool // trusted: the services whose messages go through
	until   string          // when it runs out, or ""
}

func (m sessionMode) lets(addr string) bool { return m.free || m.trust[addr] }

// sessionMode asks the daemon for the agent's mode.
func (a *agent) sessionMode() (sessionMode, error) {
	v, err := a.eval("(mode)")
	if err != nil {
		return sessionMode{}, err
	}
	v = okValue(v)
	kind, _ := field(v, "mode").(puff.Keyword)
	m := sessionMode{printed: puff.Print(v), free: kind == "free", trust: map[string]bool{}}
	services, _ := field(v, "services").(puff.Vector)
	for i := range services.Len() {
		if k, ok := services.At(i).(puff.Keyword); ok {
			m.trust[string(k)] = true
		}
	}
	if t := field(v, "until"); t != nil {
		m.until = puff.Print(t)
	}
	return m, nil
}

// says is what m lets through, for the eval tool's description; "" if it
// is gated.
func (m sessionMode) says() string {
	var s string
	switch {
	case m.free:
		s = "Your session is free: everything you send, publish and evaluate goes through without review"
	case len(m.trust) > 0:
		var names []string
		for name := range m.trust {
			names = append(names, ":"+name)
		}
		slices.Sort(names)
		s = "Your session trusts " + strings.Join(names, " ") + ": your calls and sends to them go through without review, and so does code whose only :write is those"
	default:
		return ""
	}
	if m.until != "" {
		s += ", until " + m.until
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
	resp, err := c.Claim("agent", agentIface)
	var addr string
	if err == nil {
		v, _ := puff.ReadValue(resp.Value)
		if k, ok := okValue(v).(puff.Keyword); ok {
			addr = string(k)
		} else {
			err = fmt.Errorf("claiming an agent's address: %s", resp.Value)
		}
	}
	if err != nil {
		c.Close()
		return nil, "", err
	}
	a.c, a.addr = c, addr
	return c, addr, nil
}

// dialAgent connects to the daemon as an agent, starting it if need be.
func dialAgent(dir string) (*rpc.Client, error) {
	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		return nil, err
	}
	if err := c.Compatible(); err != nil {
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
	resp, err := c.Eval(src, a.wd)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		return nil, err
	}
	return puff.ReadValue(resp.Value)
}

// fnTool is a service function as a tool.
type fnTool struct {
	tool   *mcp.Tool
	target string // address/fn
	params []string
}

// functions are every address's functions as tools, but the agent's own
// and :review's, which only you may use, described as mode treats them.
func (a *agent) functions(mode sessionMode) ([]fnTool, error) {
	v, err := a.eval("(addresses)")
	if err != nil {
		return nil, err
	}
	_, self, _ := a.client()
	addrs, _ := okValue(v).(puff.Vector)
	var tools []fnTool
	taken := map[string]bool{}
	for i := range addrs.Len() {
		addr, _ := field(addrs.At(i), "address").(puff.Keyword)
		if addr == "" || string(addr) == self || addr == "review" {
			continue
		}
		v, err := a.eval("(describe " + puff.Print(addr) + ")")
		if err != nil {
			return nil, err
		}
		fns, _ := okValue(v).(puff.Vector)
		for j := range fns.Len() {
			f := fns.At(j)
			fn, _ := field(f, "fn").(puff.Keyword)
			doc, _ := field(f, "doc").(puff.String)
			mark, _ := field(f, "mark").(puff.Keyword)
			form, _ := field(f, "form").(puff.Keyword)
			ps, _ := field(f, "params").(puff.Vector)
			t := fnTool{target: string(addr) + "/" + string(fn)}
			name := toolName(t.target)
			if taken[name] {
				continue
			}
			taken[name] = true
			props := map[string]any{}
			for k := range ps.Len() {
				p, _ := ps.At(k).(puff.Keyword)
				t.params = append(t.params, string(p))
				props[string(p)] = map[string]any{"type": "string", "description": "a puff value; text that doesn't read as one is a string"}
			}
			how := "Answers at once."
			switch {
			case form == "ask":
				how = "An ask: it waits for a person to answer, so you get a pending id; wait gives the answer."
			case mark == "write" && mode.lets(string(addr)):
				how = "Your session's mode lets it through without review: it runs at once."
			case mark == "write":
				how = "It waits for the user's review, so you get a pending id; wait gives the result."
			}
			t.tool = &mcp.Tool{Name: name, Title: ":" + t.target,
				Description: fmt.Sprintf("%s\n\n:%s, %s. %s", doc, t.target, puff.Print(mark), how),
				InputSchema: map[string]any{"type": "object", "properties": props, "additionalProperties": false},
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mark == "read" && form != "ask"}}
			tools = append(tools, t)
		}
	}
	return tools, nil
}

// toolName is address/fn as a tool's name: gh_prs, agent_2_tell; any
// character a name can't have is _.
func toolName(target string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, target)
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
	if mode.printed != a.mode {
		a.mode = mode.printed
		a.addEval(mode)
	}
	now := map[string]bool{}
	for _, f := range fns {
		now[f.tool.Name] = true
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
		var args map[string]json.RawMessage
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return failed("want arguments as an object: %v", err), nil
			}
		}
		var vals []puff.Value
		for _, p := range f.params {
			raw, ok := args[p]
			if !ok {
				break
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return failed("%s: want a string holding a puff value, got %s", p, raw), nil
			}
			vals = append(vals, shellArg(s))
		}
		c, _, err := a.client()
		if err != nil {
			return failed("%v", err), nil
		}
		resp, err := c.Message("call", f.target, puff.Print(puff.NewVector(vals...)), a.wd)
		if err != nil {
			return failed("%v", err), nil
		}
		a.noticeMode()
		return resultOf(resp.Value), nil
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
	changed := mode.printed != a.mode
	a.mu.Unlock()
	if changed {
		a.refresh()
	}
}

// addEval adds eval, described for mode.
func (a *agent) addEval(mode sessionMode) {
	how := "Code that reaches anything :write waits for the user's review as a whole: you get a pending id, and wait gives the result."
	if s := mode.says(); s != "" {
		how = s + " Other code that reaches :write waits for the user's review as a whole: you get a pending id, and wait gives the result."
		if mode.free {
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
			r := resultOf(resp.Value)
			if resp.Out != "" {
				r.Content = append([]mcp.Content{&mcp.TextContent{Text: resp.Out}}, r.Content...)
			}
			return r, nil
		})
}

// addFixed adds the tools that aren't a service's and don't change with
// the mode: wait and inbox. refresh adds eval.
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
		return resultOf(ans.resp.Value)
	case <-time.After(d):
		return toolText(fmt.Sprintf("%d is still pending: wait again later", id))
	case <-ctx.Done():
		return failed("cancelled")
	}
}

// resultOf is a printed Result as a tool's result: the value of (ok v), a
// string as it is; a pending (err e) with how to wait for it; any other
// (err e) as an error. A value that isn't a Result is printed as it is.
func resultOf(printed string) *mcp.CallToolResult {
	v, err := puff.ReadValue(printed)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 || r.Name() != "ok" && r.Name() != "err" {
		return toolText(printed)
	}
	if r.Name() == "ok" {
		if s, ok := r.At(0).(puff.String); ok {
			return toolText(string(s))
		}
		return toolText(puff.Print(r.At(0)))
	}
	if puff.Equal(field(r.At(0), "kind"), puff.Keyword("pending")) {
		msg, _ := field(r.At(0), "message").(puff.String)
		return toolText(fmt.Sprintf("pending %s: %s; call wait with this id for the result", puff.Print(field(r.At(0), "id")), msg))
	}
	res := toolText(puff.Print(r.At(0)))
	res.IsError = true
	return res
}

func toolText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func failed(format string, args ...any) *mcp.CallToolResult {
	r := toolText(fmt.Sprintf(format, args...))
	r.IsError = true
	return r
}

// okValue is v's value if it is (ok v), else nil.
func okValue(v puff.Value) puff.Value {
	if t, ok := v.(puff.Tag); ok && t.Name() == "ok" && t.Len() == 1 {
		return t.At(0)
	}
	return nil
}

// field is m's value for the keyword key, if m is a map that has it.
func field(m puff.Value, key string) puff.Value {
	if mm, ok := m.(puff.Map); ok {
		v, _ := mm.Get(puff.Keyword(key))
		return v
	}
	return nil
}

// mcpRun is whiff mcp: mcpServe on stdin and stdout.
func mcpRun(dir string, stderr io.Writer) int {
	if err := mcpServe(context.Background(), dir, &mcp.StdioTransport{}); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}
