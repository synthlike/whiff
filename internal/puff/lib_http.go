package puff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout is how long a request may take when it doesn't say.
const httpTimeout = 30 * time.Second

// Both are :write: a request goes out of the machine, and even a GET can
// carry what it read in its URL.
var httpFns = []*Builtin{
	{Name: "http/request", Args: "url [opts]", Mark: MarkWrite, Doc: "Send an HTTP request to url: (ok {:status :headers :body}), whatever the status, with header names in lower case. Options: :method, a keyword (:get unless given); :headers, a map of names to strings; :body, a string; :timeout, a duration (30s unless given). A request that fails to get an answer is (err {:kind :http}), or :timeout or :too-big.", Fn: httpRequest},
	{Name: "http/get", Args: "url [opts]", Mark: MarkWrite, Doc: "GET url as http/request does: (ok body) for a 2xx status, else (err {:kind :status :status :body}).", Fn: httpGet},
}

func httpGet(c *Call, args []Value) (Value, error) {
	if len(args) > 1 {
		opts, err := options(args, 1, "headers", "timeout")
		if err != nil {
			return nil, err
		}
		args = []Value{args[0], opts}
	}
	r, err := httpRequest(c, args)
	if err != nil {
		return nil, err
	}
	t := r.(Tag)
	if t.Name() != "ok" {
		return r, nil
	}
	res := t.At(0).(Map)
	status, _ := res.Get(Keyword("status"))
	body, _ := res.Get(Keyword("body"))
	if s := status.(Int); s >= 200 && s < 300 {
		return Ok(body), nil
	}
	e, _ := NewMap(
		Entry{Keyword("kind"), Keyword("status")},
		Entry{Keyword("message"), String(fmt.Sprintf("GET %s: %d %s", args[0].(String), status, http.StatusText(int(status.(Int)))))},
		Entry{Keyword("status"), status},
		Entry{Keyword("body"), body},
	)
	return NewTag("err", e), nil
}

func httpRequest(c *Call, args []Value) (Value, error) {
	u, err := arg[String](args, 0, "a url string")
	if err != nil {
		return nil, err
	}
	if p, err := url.Parse(string(u)); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return nil, fmt.Errorf("want an http or https url, got %s", Print(u))
	}
	opts, err := options(args, 1, "method", "headers", "body", "timeout")
	if err != nil {
		return nil, err
	}
	method := "GET"
	if m, ok := opts.Get(Keyword("method")); ok {
		k, ok := m.(Keyword)
		if !ok {
			return nil, fmt.Errorf("want a :method keyword, like :post, got %s", Print(m))
		}
		method = strings.ToUpper(string(k))
	}
	var body io.Reader
	if b, ok := opts.Get(Keyword("body")); ok {
		s, ok := b.(String)
		if !ok {
			return nil, fmt.Errorf("want a :body string, got %s", Print(b))
		}
		body = strings.NewReader(string(s))
	}
	timeout := Duration(httpTimeout)
	if t, ok := opts.Get(Keyword("timeout")); ok {
		if timeout, ok = t.(Duration); !ok || timeout <= 0 {
			return nil, fmt.Errorf("want a positive :timeout duration, got %s", Print(t))
		}
	}
	ctx, cancel := context.WithTimeout(c.Ctx, time.Duration(timeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, string(u), body)
	if err != nil {
		return nil, err
	}
	if h, ok := opts.Get(Keyword("headers")); ok {
		m, ok := h.(Map)
		if !ok {
			return nil, fmt.Errorf("want a :headers map, got %s", Print(h))
		}
		for _, e := range m.entries {
			var name string
			switch k := e.Key.(type) {
			case String:
				name = string(k)
			case Keyword:
				name = string(k)
			default:
				return nil, fmt.Errorf("want header names as strings or keywords, got %s", Print(e.Key))
			}
			v, ok := e.Val.(String)
			if !ok {
				return nil, fmt.Errorf("want header %s's value as a string, got %s", name, Print(e.Val))
			}
			req.Header.Set(name, string(v))
		}
	}

	res, err := http.DefaultClient.Do(req)
	var b []byte
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(res.Body, maxOutput+1))
		res.Body.Close()
	}
	switch {
	case c.Ctx.Err() != nil:
		return nil, errors.New("cancelled")
	case ctx.Err() != nil:
		return Errorf("timeout", "%s %s took longer than %s", method, u, Print(timeout)), nil
	case err != nil:
		return Errorf("http", "%s", err), nil
	case len(b) > maxOutput:
		return Errorf("too-big", "%s %s answered more than %d bytes", method, u, maxOutput), nil
	}
	headers := make([]Entry, 0, len(res.Header))
	for name, vs := range res.Header {
		headers = append(headers, Entry{String(strings.ToLower(name)), String(strings.Join(vs, ", "))})
	}
	hm, err := NewMap(headers...)
	if err != nil {
		return nil, err
	}
	m, _ := NewMap(
		Entry{Keyword("status"), Int(res.StatusCode)},
		Entry{Keyword("headers"), hm},
		Entry{Keyword("body"), String(b)},
	)
	return Ok(m), nil
}
