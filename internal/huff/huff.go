// Package huff is whiff's own agent harness: a model, a transcript and
// tools, a turn at a time (run.go), the tools files and commands in a
// directory, as claude -p's (local.go), or what its caller gives it.
// This file is its provider-neutral transcript and the providers a
// model's name picks; chat.go speaks OpenAI's Chat Completions format,
// which OpenRouter, OpenAI and local servers share. It knows nothing of
// whiff: cmd/huff gives it whiff functions as tools.
package huff

import (
	"context"
	"fmt"
	"strings"
)

// A Message is one entry of a transcript. Role is "user", "assistant"
// or "tool". A user's has Text; an assistant's Text, Calls or both; a
// tool's the ID of the call it answers and its Result, IsError if the
// call failed.
type Message struct {
	Role    string
	Text    string
	Calls   []Call
	ID      string
	Result  string
	IsError bool
}

// A Call is a tool call an assistant asked for: its ID, which the tool's
// answer names, the tool Fn, and its Args, a JSON object as the model
// wrote it.
type Call struct {
	ID, Fn, Args string
}

// A Tool is what the model may call: Name, what it does, and Params, a
// JSON Schema object for its arguments.
type Tool struct {
	Name, Description string
	Params            map[string]any
}

// Usage is what a model call took: tokens in and out, and Cost in
// dollars where the provider says it, as OpenRouter does; 0 otherwise.
type Usage struct {
	Input, Output int
	Cost          float64
}

// Add is u and v together.
func (u Usage) Add(v Usage) Usage {
	return Usage{Input: u.Input + v.Input, Output: u.Output + v.Output, Cost: u.Cost + v.Cost}
}

// A Request is one model call: the model, as its provider names it, the
// system prompt, the transcript so far and the tools it may call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int // 0 for the provider's default
}

// A Reply is the model's answer: an assistant Message, why it stopped,
// and what it took. Stop is "end" (it answered), "tools" (it wants its
// Calls run), "length" (it ran out of tokens) or "filter" (the provider
// withheld the answer); anything else as the provider said it.
type Reply struct {
	Message Message
	Stop    string
	Usage   Usage
}

// A Provider makes model calls.
type Provider interface {
	Complete(ctx context.Context, r Request) (Reply, error)
}

// Open is the provider and model a model's name picks, its first part
// naming the provider and the rest the model as that provider names it:
//
//	openrouter/moonshotai/kimi-k2  OpenRouter, with OPENROUTER_API_KEY
//	openai/<model>                 OpenAI, with OPENAI_API_KEY
//	local/<model>                  a server on this machine that speaks
//	                               Chat Completions, at WHIFF_LOCAL_URL,
//	                               Ollama's http://localhost:11434/v1 if unset
//
// getenv is os.Getenv, or a test's.
func Open(name string, getenv func(string) string) (Provider, string, error) {
	provider, model, ok := strings.Cut(name, "/")
	if !ok || model == "" {
		return nil, "", fmt.Errorf("want a model as provider/model, as openrouter/moonshotai/kimi-k2, got %q", name)
	}
	keyed := func(base, env string) (Provider, string, error) {
		key := getenv(env)
		if key == "" {
			return nil, "", fmt.Errorf("%s needs %s set in whiff's environment", provider, env)
		}
		return &Chat{BaseURL: base, Key: key}, model, nil
	}
	switch provider {
	case "openrouter":
		return keyed("https://openrouter.ai/api/v1", "OPENROUTER_API_KEY")
	case "openai":
		return keyed("https://api.openai.com/v1", "OPENAI_API_KEY")
	case "local":
		base := getenv("WHIFF_LOCAL_URL")
		if base == "" {
			base = "http://localhost:11434/v1"
		}
		return &Chat{BaseURL: strings.TrimSuffix(base, "/")}, model, nil
	}
	return nil, "", fmt.Errorf("no provider %q: want openrouter, openai or local", provider)
}
