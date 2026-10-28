package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/daemon"
	"github.com/synthlike/whiff/internal/rpc"
)

// TestMain runs sniff itself when SNIFF_TEST_DIR names a daemon's socket
// directory, to look at the page against a throwaway daemon.
func TestMain(m *testing.M) {
	if dir := os.Getenv("SNIFF_TEST_DIR"); dir != "" {
		os.Exit(run(os.Args[1:], dir, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestTree(t *testing.T) {
	tests := []struct{ printed, want string }{
		{`(ok {:n 1 :s "x"})`, `{"t":"tag","n":"ok","v":[{"t":"map","v":[[{"t":"kw","v":"n"},{"t":"lit","v":"1"}],[{"t":"kw","v":"s"},{"t":"str","v":"x"}]]}]}`},
		{`[1.5 #t 2s]`, `{"t":"vec","v":[{"t":"lit","v":"1.5"},{"t":"lit","v":"#t"},{"t":"lit","v":"2s"}]}`},
		{`(nope`, `{"t":"lit","v":"(nope"}`},
	}
	for _, tt := range tests {
		got, _ := json.Marshal(treeOf(tt.printed))
		if string(got) != tt.want {
			t.Errorf("treeOf(%s) = %s, want %s", tt.printed, got, tt.want)
		}
	}
}

// sniffing starts a daemon with services, and sniff on it; it returns
// sniff, its base URL and a client of the daemon.
func sniffing(t *testing.T, services map[string]string) (*server, string, *rpc.Client) {
	t.Helper()
	// A short directory: a unix socket path must fit in about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "sniff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	svc := filepath.Join(dir, "services")
	os.Mkdir(svc, 0o700)
	for name, src := range services {
		os.WriteFile(filepath.Join(svc, name+".puff"), []byte(src), 0o600)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { daemon.Run(ctx, dir, svc); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	var c *rpc.Client
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err = rpc.Dial(dir, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { c.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(dir, ln.Addr().(*net.TCPAddr).Port)
	go s.follow(ctx)
	srv := &http.Server{Handler: s.handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return s, fmt.Sprintf("http://127.0.0.1:%d", s.port), c
}

func TestGuard(t *testing.T) {
	s, base, _ := sniffing(t, nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url, host string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		if host != "" {
			req.Host = host
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get(base+"/", "", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("no token: %s", r.Status)
	}
	if r := get(base+"/?token=nope", "", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong token: %s", r.Status)
	}
	if r := get(base+"/?token="+s.token, fmt.Sprintf("evil.example:%d", s.port), nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("another host: %s", r.Status)
	}
	r := get(base+"/?token="+s.token, "", nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" || len(r.Cookies()) != 1 {
		t.Fatalf("the token: %s %v %v", r.Status, r.Header.Get("Location"), r.Cookies())
	}
	cookie := r.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v", cookie)
	}
	if r := get(base+"/", "", cookie); r.StatusCode != http.StatusOK || r.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("with the cookie: %s %v", r.Status, r.Header)
	}
	if r := get(base+"/", fmt.Sprintf("localhost:%d", s.port), cookie); r.StatusCode != http.StatusOK {
		t.Errorf("as localhost: %s", r.Status)
	}
}

func TestSniff(t *testing.T) {
	s, base, c := sniffing(t, map[string]string{
		"greet":   `(srv greet (on (hello name) "Greets name." (ok (str "hello, " name))))`,
		"counter": `(srv counter :state {:n 1} (on (get s) "The count." (ok s)))`,
	})
	cookie := &http.Cookie{Name: fmt.Sprintf("sniff-%d", s.port), Value: s.token}
	get := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	body := func(path string) string {
		t.Helper()
		resp := get(path)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s %s", path, resp.Status, b)
		}
		return string(b)
	}

	// The page, and its events: a snapshot, then each message.
	if !strings.Contains(body("/"), "<title>sniff</title>") {
		t.Error("no page at /")
	}
	if !strings.Contains(body("/app.js"), "EventSource") {
		t.Error("no app.js")
	}
	events := get("/events")
	defer events.Body.Close()
	lines := bufio.NewScanner(events.Body)
	next := func() (string, string) {
		t.Helper()
		var name string
		for lines.Scan() {
			switch l := lines.Text(); {
			case strings.HasPrefix(l, "event: "):
				name = l[7:]
			case strings.HasPrefix(l, "data: "):
				return name, l[6:]
			}
		}
		t.Fatal("events ended")
		return "", ""
	}
	// Until sniff follows the daemon, a snapshot may say it doesn't yet.
	for {
		name, data := next()
		if strings.Contains(data, `"connected":true`) {
			if name != "snapshot" && name != "status" {
				t.Fatalf("event %s %s", name, data)
			}
			break
		}
	}
	if _, err := c.Message("call", "greet/hello", `["ann"]`, ""); err != nil {
		t.Fatal(err)
	}
	for {
		name, data := next()
		if name != "entry" {
			continue
		}
		var e struct {
			Kind, From, To string
			Args, Reply    *tree
		}
		json.Unmarshal([]byte(data), &e)
		if e.Kind != "call" || e.To != "greet/hello" {
			continue
		}
		if !strings.Contains(data, `"args":{"t":"vec","v":[{"t":"str","v":"ann"}]}`) ||
			!strings.Contains(data, `"reply":{"t":"tag","n":"ok","v":[{"t":"str","v":"hello, ann"}]}`) {
			t.Errorf("entry = %s", data)
		}
		break
	}

	// Addresses, with their functions, and a service's state.
	var addrs []struct {
		Address       string
		Client, State bool
		Fns           []map[string]any
	}
	if err := json.Unmarshal([]byte(body("/api/addresses")), &addrs); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addrs {
		if a.Address == "greet" {
			found = true
			if len(a.Fns) != 1 || a.Fns[0]["fn"] != "hello" || a.Fns[0]["mark"] != "read" || a.Fns[0]["doc"] != "Greets name." {
				t.Errorf("greet = %+v", a)
			}
		}
	}
	if !found {
		t.Errorf("no :greet in %+v", addrs)
	}
	if got := strings.TrimSpace(body("/api/state?address=counter")); got != `{"t":"tag","n":"ok","v":[{"t":"map","v":[[{"t":"kw","v":"n"},{"t":"lit","v":"1"}]]}]}` {
		t.Errorf("state = %s", got)
	}
	if r := get("/api/state?address=" + "bad%20name"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad address: %s", r.Status)
	}
}
