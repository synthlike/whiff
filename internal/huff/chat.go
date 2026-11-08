package huff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Backoff is the first wait before trying a call again; a test's is
// shorter.
var Backoff = time.Second

// Chat is a provider that speaks OpenAI's Chat Completions format,
// POST {BaseURL}/chat/completions, as OpenRouter, OpenAI and local
// servers do.
type Chat struct {
	BaseURL string
	Key     string       // sent as a bearer token; "" for none, as a local server
	HTTP    *http.Client // nil for one with a 10 minute timeout
	// Retries is how many times a call is tried again after a 429, a 402, a 5xx,
	// a failed connection or a 200 that isn't a chat completion, waiting
	// as the server asks or 1s, 2s, 4s, ...; 0 for 4, -1 for none. A turn
	// that fails loses its work so far, so it tries hard.
	Retries int
}

// The wire format, only the fields huff uses.
type (
	chatRequest struct {
		Model     string        `json:"model"`
		Messages  []chatMessage `json:"messages"`
		Tools     []chatTool    `json:"tools,omitempty"`
		MaxTokens int           `json:"max_tokens,omitempty"`
	}
	chatMessage struct {
		Role       string         `json:"role"`
		Content    *string        `json:"content"` // null for an assistant's that only calls tools
		ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
	}
	chatToolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	chatTool struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description,omitempty"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	chatResponse struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			Cost             float64 `json:"cost"` // OpenRouter's, in dollars
		} `json:"usage"`
		Error *chatError `json:"error"`
	}
	chatError struct {
		Message string `json:"message"`
		Code    any    `json:"code"` // a number on OpenRouter, a string on OpenAI
	}
)

// Complete makes one model call.
func (c *Chat) Complete(ctx context.Context, r Request) (Reply, error) {
	body, err := json.Marshal(chatRequestOf(r))
	if err != nil {
		return Reply{}, err
	}
	retries := c.Retries
	switch {
	case retries == 0:
		retries = 4
	case retries < 0:
		retries = 0
	}
	for try := 0; ; try++ {
		reply, wait, err := c.post(ctx, body)
		if wait < 0 || try >= retries {
			return reply, err
		}
		if wait == 0 {
			wait = Backoff << try
		}
		select {
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// post sends body once: the reply, or why not and how long to wait
// before trying again, 0 for the default, -1 for a failure that trying
// again won't mend.
func (c *Chat) post(ctx context.Context, body []byte) (Reply, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Reply{}, -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Reply{}, -1, ctx.Err()
		}
		return Reply{}, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Reply{}, 0, err
	}
	var cr chatResponse
	jerr := json.Unmarshal(data, &cr)
	if resp.StatusCode != http.StatusOK {
		msg := http.StatusText(resp.StatusCode)
		if jerr == nil && cr.Error != nil && cr.Error.Message != "" {
			msg = cr.Error.Message
		}
		err := fmt.Errorf("%s: %d %s", c.BaseURL, resp.StatusCode, msg)
		// OpenRouter answers 402 when the requests in flight would spend
		// more than the credits left, and says to try once they settle.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode >= 500 {
			wait := time.Duration(0)
			if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			return Reply{}, wait, err
		}
		return Reply{}, -1, err
	}
	if jerr != nil {
		return Reply{}, 0, fmt.Errorf("%s: want a chat completion, got %.200q: %v", c.BaseURL, data, jerr)
	}
	// OpenRouter may answer 200 with an error from the model's provider.
	if cr.Error != nil {
		return Reply{}, 0, fmt.Errorf("%s: %s", c.BaseURL, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Reply{}, 0, errors.New(c.BaseURL + ": a chat completion with no choices")
	}
	ch := cr.Choices[0]
	m := Message{Role: "assistant"}
	if ch.Message.Content != nil {
		m.Text = *ch.Message.Content
	}
	for _, tc := range ch.Message.ToolCalls {
		m.Calls = append(m.Calls, Call{ID: tc.ID, Fn: tc.Function.Name, Args: tc.Function.Arguments})
	}
	stop := ch.FinishReason
	switch stop {
	case "stop":
		stop = "end"
	case "tool_calls":
		stop = "tools"
	case "content_filter":
		stop = "filter"
	}
	// Some servers say stop though they called tools.
	if len(m.Calls) > 0 && stop == "end" {
		stop = "tools"
	}
	return Reply{Message: m, Stop: stop,
		Usage: Usage{Input: cr.Usage.PromptTokens, Output: cr.Usage.CompletionTokens, Cost: cr.Usage.Cost}}, -1, nil
}

// chatRequestOf is r in the wire format.
func chatRequestOf(r Request) chatRequest {
	text := func(s string) *string { return &s }
	cr := chatRequest{Model: r.Model, MaxTokens: r.MaxTokens}
	if r.System != "" {
		cr.Messages = append(cr.Messages, chatMessage{Role: "system", Content: text(r.System)})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "assistant":
			cm := chatMessage{Role: "assistant"}
			if m.Text != "" || len(m.Calls) == 0 {
				cm.Content = text(m.Text)
			}
			for _, c := range m.Calls {
				tc := chatToolCall{ID: c.ID, Type: "function"}
				tc.Function.Name, tc.Function.Arguments = c.Fn, c.Args
				cm.ToolCalls = append(cm.ToolCalls, tc)
			}
			cr.Messages = append(cr.Messages, cm)
		case "tool":
			result := m.Result
			if m.IsError {
				result = "error: " + result
			}
			cr.Messages = append(cr.Messages, chatMessage{Role: "tool", ToolCallID: m.ID, Content: text(result)})
		default:
			cr.Messages = append(cr.Messages, chatMessage{Role: m.Role, Content: text(m.Text)})
		}
	}
	for _, t := range r.Tools {
		ct := chatTool{Type: "function"}
		ct.Function.Name, ct.Function.Description, ct.Function.Parameters = t.Name, t.Description, t.Params
		if ct.Function.Parameters == nil {
			ct.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		cr.Tools = append(cr.Tools, ct)
	}
	return cr
}
