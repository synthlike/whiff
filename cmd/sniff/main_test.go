package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	notes := filepath.Join(t.TempDir(), "notes")
	s, base, c := sniffing(t, map[string]string{
		"greet":   `(service greet (on (hello name) "Greets name." (ok (str "hello, " name))))`,
		"counter": `(service counter :state {:n 1} (on (get s) "The count." (ok s)))`,
		"notes":   `(service notes (on (add text) "Appends text." (file/append "` + notes + `" text)))`,
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

	// An agent session's writes, each with its gate, and their summary.
	agent, err := rpc.Dial(s.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	agent.Agent = true
	if resp, err := agent.Claim("agent", `(ask (tell text) "Asks the agent.")`); err != nil || resp.Value != "(ok :agent/1)" {
		t.Fatalf("claim = %+v %v", resp, err)
	}
	if _, err := c.Mode("agent/1", "free", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Message("call", "notes/add", `["a"]`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Message("call", "greet/hello", `["ann"]`, ""); err != nil {
		t.Fatal(err)
	}
	var session struct {
		Entries []struct{ From, To, Gate string }
		Summary string
	}
	if err := json.Unmarshal([]byte(body("/api/session?address=agent/1")), &session); err != nil {
		t.Fatal(err)
	}
	if len(session.Entries) != 2 || session.Entries[1].To != "notes/add" || session.Entries[1].Gate != "free" ||
		session.Summary != "agent/1: 1 write went through (1 free)" {
		t.Errorf("session = %+v", session)
	}
	if got := body("/api/session?address=agent/9"); !strings.Contains(got, `"entries":[]`) {
		t.Errorf("an unknown session = %s", got)
	}
}

// TestViews follows a view pinned as a page: fetched as you while it is
// :read, an action sent as you, and the topic that says to fetch it again.
func TestViews(t *testing.T) {
	s, base, _ := sniffing(t, map[string]string{
		"board": `(service board :state {:n 0}
  (on (show s) :read "A view."
    (ok (view/items [{:label (str "n " (map/get-or s :n 0)) :action [:board/bump 2]}] {:changed :board/changed})))
  (on (times s k) :read "n times k, as text."
    (ok (view/text (str (* (map/get-or s :n 0) k)))))
  (on (bump s k) :write :update "Adds k."
    (do (publish :board/changed {})
      (ok [:done (map/update s :n (fn (n) (+ n k)))]))))`,
	})
	cookie := &http.Cookie{Name: fmt.Sprintf("sniff-%d", s.port), Value: s.token}
	do := func(method, path, ctype, body string, header ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.AddCookie(cookie)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if r, b := do("GET", "/view/board/show", "", ""); r.StatusCode != http.StatusOK || !strings.Contains(b, "page.js") {
		t.Errorf("the page: %s %s", r.Status, b)
	}
	view := func() string {
		t.Helper()
		// Until sniff's daemon has loaded :board, it may have no such address.
		var b string
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			var r *http.Response
			if r, b = do("GET", "/api/view?target=board/show", "", ""); r.StatusCode == http.StatusOK {
				return b
			}
		}
		t.Fatalf("view: %s", b)
		return ""
	}
	if got := view(); !strings.Contains(got, `"n":"items"`) || !strings.Contains(got, `{"t":"str","v":"n 0"}`) {
		t.Errorf("view = %s", got)
	}
	for _, tt := range []struct {
		target string
		code   int
	}{{"board/bump", http.StatusForbidden}, {"board/nope", http.StatusNotFound}, {"bad%20name", http.StatusBadRequest}} {
		if r, b := do("GET", "/api/view?target="+tt.target, "", ""); r.StatusCode != tt.code {
			t.Errorf("view %s: %s %s, want %d", tt.target, r.Status, b, tt.code)
		}
	}

	// The watch, a stream that says when the view changed, on any of its
	// topics.
	req, _ := http.NewRequest("GET", base+"/api/watch?topic=board/other&topic=board/changed", nil)
	req.AddCookie(cookie)
	watch, err := http.DefaultClient.Do(req)
	if err != nil || watch.StatusCode != http.StatusOK {
		t.Fatalf("watch: %v %v", watch, err)
	}
	defer watch.Body.Close()
	lines := bufio.NewScanner(watch.Body)
	if !lines.Scan() || lines.Text() != ": following" {
		t.Fatalf("watch began %q", lines.Text())
	}

	// A form posts its action, from sniff's own page only, and goes back to
	// the page, which shows what it answered.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(form url.Values, header ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", base+"/act", strings.NewReader(form.Encode()))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	form := url.Values{"action": {"[:board/bump 2]"}, "back": {"/view/board/show"}}
	if r := post(form, "Sec-Fetch-Site", "cross-site"); r.StatusCode != http.StatusForbidden {
		t.Errorf("another site's form: %s", r.Status)
	}
	if r := post(form, "Origin", "http://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Errorf("another origin's form: %s", r.Status)
	}
	if r := post(url.Values{"action": {"[1]"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad action: %s", r.Status)
	}
	// As a browser posts it from sniff's page, its Origin may be null.
	r := post(form, "Sec-Fetch-Site", "same-origin", "Origin", "null")
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/view/board/show?said=1" {
		t.Fatalf("the form: %s %s", r.Status, r.Header.Get("Location"))
	}
	got := make(chan string, 1)
	go func() {
		for lines.Scan() {
			if strings.HasPrefix(lines.Text(), "event: ") {
				got <- lines.Text()
				return
			}
		}
	}()
	select {
	case e := <-got:
		if e != "event: changed" {
			t.Errorf("watch: %q", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no change on the watch")
	}
	if got := view(); !strings.Contains(got, `{"t":"str","v":"n 2"}`) || !strings.Contains(got, `[{"t":"kw","v":"changed"},{"t":"kw","v":"board/changed"}]`) {
		t.Errorf("view after acting = %s", got)
	}

	// A :read function's view with args, as a :read action links to it.
	if r, b := do("GET", "/api/view?target=board/times&args="+url.QueryEscape("[3]"), "", ""); r.StatusCode != http.StatusOK ||
		strings.TrimSpace(b) != `{"t":"tag","n":"ok","v":[{"t":"tag","n":"text","v":[{"t":"str","v":"6"}]}]}` {
		t.Errorf("view with args: %s %s", r.Status, b)
	}
	for _, args := range []string{"3", "[3"} {
		if r, b := do("GET", "/api/view?target=board/times&args="+url.QueryEscape(args), "", ""); r.StatusCode != http.StatusBadRequest {
			t.Errorf("args %s: %s %s", args, r.Status, b)
		}
	}

	// The page sniff draws: the view, its action a form; and the view
	// alone, for the page to swap in.
	if r, b := do("GET", "/view/board/show", "", ""); r.StatusCode != http.StatusOK || !strings.Contains(b, `data-topics="board/changed"`) ||
		!strings.Contains(b, `<span>n 2</span>`) || !strings.Contains(b, `<input type="hidden" name="action" value="[:board/bump 2]">`) {
		t.Errorf("the page: %s %s", r.Status, b)
	}
	if r, b := do("GET", "/view/board/show?part=view", "", ""); r.StatusCode != http.StatusOK || strings.Contains(b, "<html") || !strings.Contains(b, `<span>n 2</span>`) {
		t.Errorf("the view alone: %s %s", r.Status, b)
	}
	if r, b := do("GET", "/view/board/bump", "", ""); r.StatusCode != http.StatusForbidden || !strings.Contains(b, "a page fetches only :read functions") {
		t.Errorf("a :write function's page: %s %s", r.Status, b)
	}

	// Back on the page, it shows what the form answered.
	if _, b := do("GET", "/view/board/show?said=1", "", ""); !strings.Contains(b, `<p class="said ok">Sent <code>[:board/bump 2]</code>: (ok :done)</p>`) ||
		!strings.Contains(b, `<span>n 2</span>`) {
		t.Errorf("back on the page: %s", b)
	}
	// A tab of the main page gets the view with its topics, and what the
	// form answered, without the page around it.
	if r, b := do("GET", "/view/board/show?part=panel&said=1", "", ""); r.StatusCode != http.StatusOK || strings.Contains(b, "<html") ||
		!strings.Contains(b, `<div class="shown" data-topics="board/changed">`) || !strings.Contains(b, `<p class="said ok">`) ||
		!strings.Contains(b, `name="back" value="/view/board/show"`) {
		t.Errorf("the view as a tab: %s %s", r.Status, b)
	}
	// One that asks gets your answer as its last argument; back never
	// leaves sniff.
	r = post(url.Values{"action": {"[:board/bump]"}, "answer": {" 1 "}, "back": {"//evil.example/"}})
	if r.Header.Get("Location") != "/?said=2" {
		t.Errorf("a form going elsewhere: %s", r.Header.Get("Location"))
	}
	s.mu.Lock()
	sd := s.said[2]
	s.mu.Unlock()
	if sd.Sends != `[:board/bump "1"]` || sd.OK {
		t.Errorf("an answered form sent %+v", sd)
	}

	// A past reply, drawn for the main page: its links, but no forms, as
	// what it offered may no longer hold.
	reply := `(ok (items [{:label "x" :action [:board/bump 2] :actions [{:label "Times" :action [:board/times 3]}]}]))`
	if r, b := do("POST", "/render?as=reply", "text/plain", reply); r.StatusCode != http.StatusOK || strings.Contains(b, "<form") ||
		!strings.Contains(b, `href="/view/board/times?args=%5B3%5D"`) || !strings.Contains(b, `<span>x</span>`) {
		t.Errorf("a reply drawn: %s %s", r.Status, b)
	}
	if _, b := do("POST", "/render", "text/plain", `[1 "two"]`); !strings.Contains(b, `<span class="str">&#34;two&#34;</span>`) {
		t.Errorf("a value drawn: %s", b)
	}
}
