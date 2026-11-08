package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/synthlike/whiff/internal/huff"
	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
	"github.com/synthlike/whiff/internal/tools"
)

// sessionIface is what the agent's address offers others while huff holds
// its session, as whiff mcp's: messages to it wait in its inbox.
const sessionIface = `(ask (tell text) "Leaves the agent a message, which it reads from its inbox and answers when it next looks.")`

// session is an agent's whiff session, claimed with a job's token: its
// tools are the functions the agent may use, called as the session, so a
// :write waits for your review and history has each call as the agent's.
type session struct {
	c    *rpc.Client
	wd   string
	fns  []tools.Fn
	byFn map[string]tools.Fn // by address/fn, for the actions an answer offers
}

// openSession claims the session token names from the daemon in dir, the
// default one if "", acting in wd, and describes its functions as tools.
func openSession(dir, token, wd string) (*session, error) {
	if dir == "" {
		dir = rpc.DefaultDir()
	}
	c, err := rpc.Dial(dir, nil)
	if err != nil {
		return nil, fmt.Errorf("whiff: %v", err)
	}
	if err := c.Compatible("huff"); err != nil {
		c.Close()
		return nil, err
	}
	c.Agent = true
	resp, err := c.ClaimSession(token, sessionIface)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("claiming the session: %v", err)
	}
	v, _ := puff.ReadValue(resp.Value)
	claimed, _ := puff.OkValue(v).(puff.Vector)
	if claimed.Len() != 2 {
		c.Close()
		return nil, fmt.Errorf("claiming the session: %s", resp.Value)
	}
	self, _ := claimed.At(0).(puff.Keyword)
	fnv, _ := claimed.At(1).(puff.Vector)
	can := map[string]bool{}
	for i := range fnv.Len() {
		if k, ok := fnv.At(i).(puff.Keyword); ok {
			can[string(k)] = true
		}
	}
	eval := func(src string) (puff.Value, error) { return c.EvalValue(src, wd) }
	mv, err := eval("(session/mode)")
	if err != nil {
		c.Close()
		return nil, err
	}
	fns, err := tools.Describe(eval, string(self), can, tools.ModeOf(mv))
	if err != nil {
		c.Close()
		return nil, err
	}
	s := &session{c: c, wd: wd, fns: fns, byFn: map[string]tools.Fn{}}
	for _, f := range fns {
		s.byFn[f.Target] = f
	}
	return s, nil
}

func (s *session) close() { s.c.Close() }

// Tools are the session's functions as the model sees them.
func (s *session) Tools() []huff.Tool {
	ts := make([]huff.Tool, len(s.fns))
	for i, f := range s.fns {
		ts[i] = huff.Tool{Name: f.Name, Description: f.Description, Params: f.Schema}
	}
	return ts
}

// Call calls the function c names as the session.
func (s *session) Call(_ context.Context, c huff.Call) (string, bool) {
	var f tools.Fn
	found := false
	for _, g := range s.fns {
		if g.Name == c.Fn {
			f, found = g, true
			break
		}
	}
	if !found {
		return fmt.Sprintf("there is no tool %s", c.Fn), true
	}
	args, err := tools.Args(f, []byte(c.Args))
	if err != nil {
		return err.Error(), true
	}
	resp, err := s.c.Message("call", f.Target, puff.Print(args), s.wd)
	if err != nil {
		return err.Error(), true
	}
	text, isErr := tools.Result(resp.Value, "don't wait for it: its result comes to you later, as a message of its own")
	if !isErr {
		if lines := tools.Offers(resp.Value, s.byFn); len(lines) > 0 {
			text += "\n\nWhat you can do from here, each as a tool call:\n" + strings.Join(lines, "\n")
		}
	}
	return text, isErr
}
