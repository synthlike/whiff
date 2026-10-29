// sniff is whiff's web UI: a page, served on 127.0.0.1, that shows what
// goes on between whiff's addresses as it happens, and each address's
// functions and state. It is a client of the daemon like the CLI and hiss,
// never part of it.
//
//	sniff [-port n] [-open]
//
// It prints the page's address, with a token that only you have: the page
// is yours, as the socket is, and what it sends is sent as you, as from the
// CLI. A :read function's view can be a page of its own,
// /view/address/fn?args=[...], fetched again each time the topic it names
// fires ({:changed topic}, ?on=topic, or address/changed). An item's
// action to a :read function is a link to its page; clicking any other
// sends it, as whiff act does.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

//go:embed web
var web embed.FS

// historyKeep bounds the history sniff keeps for pages that open later, as
// the daemon bounds its own.
const historyKeep = 1000

func main() {
	os.Exit(run(os.Args[1:], rpc.DefaultDir(), os.Stdout, os.Stderr))
}

// run is sniff with its arguments, the daemon's socket directory and its
// streams, returning the exit code. It serves until interrupted.
func run(args []string, dir string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("sniff", flag.ContinueOnError)
	fl.SetOutput(stderr)
	port := fl.Int("port", 0, "the port to listen on, on 127.0.0.1; any free one by default")
	open := fl.Bool("open", false, "open the page in your browser")
	if fl.Parse(args) != nil || fl.NArg() > 0 {
		fl.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "sniff: %v\n", err)
		return 1
	}
	s := newServer(dir, ln.Addr().(*net.TCPAddr).Port)
	url := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", s.port, s.token)
	fmt.Fprintln(stdout, url)
	if *open {
		openBrowser(url)
	}
	go s.follow(ctx)
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "sniff: %v\n", err)
		return 1
	}
	return 0
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	exec.Command(name, url).Start()
}

// server is sniff: the page, what it asks for, and history as the daemon
// traces it, which it pushes to every page open.
type server struct {
	dir   string // the daemon's socket directory
	wd    string // the directory sniff calls from, as the CLI calls from its own
	port  int
	token string

	mu       sync.Mutex
	entries  []json.RawMessage // history, oldest first, as pages get it
	status   status
	watchers map[chan event]bool
}

// status is whether sniff follows the daemon, and why not.
type status struct {
	Connected bool   `json:"connected"`
	Why       string `json:"why,omitempty"`
}

// event is a server-sent event for the pages: a snapshot when one opens,
// then each entry and status.
type event struct {
	name string
	data []byte
}

func newServer(dir string, port int) *server {
	b := make([]byte, 16)
	rand.Read(b)
	wd, _ := os.Getwd()
	return &server{dir: dir, wd: wd, port: port, token: hex.EncodeToString(b),
		status: status{Why: "connecting to whiff"}, watchers: map[chan event]bool{}}
}

// follow traces the daemon's history until ctx ends, connecting again a
// second after it stops: sniff doesn't start it, as the CLI does.
func (s *server) follow(ctx context.Context) {
	for {
		err := s.trace(ctx)
		if ctx.Err() != nil {
			return
		}
		s.setStatus(status{Why: err.Error()})
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// trace follows the daemon until its connection ends, and says why.
func (s *server) trace(ctx context.Context) error {
	c, err := s.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	es, err := c.Trace()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.entries = s.entries[:0]
	for _, e := range es {
		s.entries = append(s.entries, entryJSON(e))
	}
	s.status = status{Connected: true}
	s.mu.Unlock()
	s.broadcast(event{"snapshot", s.snapshot()})
	for {
		select {
		case e, ok := <-c.Entries():
			if !ok {
				return errors.New("whiff stopped")
			}
			s.add(entryJSON(e))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dial connects to the daemon, if it runs and speaks this sniff's
// protocol.
func (s *server) dial() (*rpc.Client, error) {
	c, err := rpc.Dial(s.dir, nil)
	if err != nil {
		return nil, errors.New("whiff isn't running")
	}
	if err := c.Compatible(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (s *server) add(e json.RawMessage) {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	if n := len(s.entries) - historyKeep; n > 0 {
		s.entries = slices.Delete(s.entries, 0, n)
	}
	s.mu.Unlock()
	s.broadcast(event{"entry", e})
}

func (s *server) setStatus(st status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
	data, _ := json.Marshal(st)
	s.broadcast(event{"status", data})
}

// snapshot is what a page gets when it opens: {:status :entries}.
func (s *server) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(struct {
		Status  status            `json:"status"`
		Entries []json.RawMessage `json:"entries"`
	}{s.status, s.entries})
	return data
}

// broadcast sends e to every page. One that doesn't keep up is closed: its
// browser opens it again and gets a fresh snapshot.
func (s *server) broadcast(e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.watchers {
		select {
		case w <- e:
		default:
			delete(s.watchers, w)
			close(w)
		}
	}
}

// handler is sniff's routes, behind its checks.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(web, "web")
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /view/", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, files, "view.html") })
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /api/addresses", s.addresses)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/view", s.view)
	mux.HandleFunc("GET /api/watch", s.watch)
	mux.HandleFunc("POST /api/act", s.act)
	return s.guard(mux)
}

// guard lets a request through only for its Host, 127.0.0.1 or localhost
// at sniff's port, against DNS rebinding; and only with sniff's token. The
// token comes once in the URL, which trades it for a cookie and drops it.
func (s *server) guard(next http.Handler) http.Handler {
	cookie := fmt.Sprintf("sniff-%d", s.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != fmt.Sprintf("127.0.0.1:%d", s.port) && r.Host != fmt.Sprintf("localhost:%d", s.port) {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if t := r.URL.Query().Get("token"); t != "" {
			if !s.valid(t) {
				http.Error(w, "wrong token", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(cookie); err != nil || !s.valid(c.Value) {
			http.Error(w, "open the address sniff printed, with its token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) valid(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

// events streams server-sent events to a page: a snapshot, then entries
// and statuses as they come.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	ch := make(chan event, 256)
	s.mu.Lock()
	s.watchers[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.watchers[ch] {
			delete(s.watchers, ch)
			close(ch)
		}
		s.mu.Unlock()
	}()
	send := func(e event) bool {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		return rc.Flush() == nil
	}
	if !send(event{"snapshot", s.snapshot()}) {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok || !send(e) {
				return
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			if rc.Flush() != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// addresses answers every address with its functions: [{"address" "client"
// "state" "fns": [{"fn" "params" "doc" "mark" "form"}]}].
func (s *server) addresses(w http.ResponseWriter, r *http.Request) {
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	addrs, err := evalOk(c, "(addresses)")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out := []any{}
	vec, _ := addrs.(puff.Vector)
	for i := range vec.Len() {
		a, _ := plain(vec.At(i)).(map[string]any)
		name, _ := a["address"].(string)
		fns, err := evalOk(c, "(describe "+puff.Print(puff.Keyword(name))+")")
		if err != nil {
			continue // gone since
		}
		a["fns"] = plain(fns)
		out = append(out, a)
	}
	writeJSON(w, out)
}

// state answers the address's state, (state :address)'s Result as a tree.
func (s *server) state(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Query().Get("address"), ":")
	if k, err := puff.ReadValue(":" + name); err != nil || name == "" || !puff.Equal(k, puff.Keyword(name)) {
		http.Error(w, "want an address", http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	resp, err := c.Eval("(state "+puff.Print(puff.Keyword(name))+")", "")
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, treeOf(resp.Value))
}

// session answers what the agent session address did, as whiff session
// shows it: {"entries": [...], "summary": "agent/3: 2 writes went through ..."}.
func (s *server) session(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Query().Get("address"), ":")
	if k, err := puff.ReadValue(":" + name); err != nil || name == "" || !puff.Equal(k, puff.Keyword(name)) {
		http.Error(w, "want an address", http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	es, err := c.History()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	got, summary := rpc.Session(es, name)
	out := struct {
		Entries []json.RawMessage `json:"entries"`
		Summary string            `json:"summary"`
	}{Entries: []json.RawMessage{}, Summary: summary}
	for _, e := range got {
		out.Entries = append(out.Entries, entryJSON(e))
	}
	writeJSON(w, out)
}

// view answers what target, address/fn, answers called as you with args, a
// printed vector (none by default), as a tree: what a pinned page shows.
// Only a :read function's, so fetching a page, again and again, changes
// nothing.
func (s *server) view(w http.ResponseWriter, r *http.Request) {
	target, ok := keyword(r.URL.Query().Get("target"))
	i := strings.LastIndex(target, "/")
	if !ok || i <= 0 {
		http.Error(w, "want a target, address/fn", http.StatusBadRequest)
		return
	}
	addr, fn := target[:i], target[i+1:]
	args := r.URL.Query().Get("args")
	if args == "" {
		args = "[]"
	}
	if v, err := puff.ReadValue(args); err != nil {
		http.Error(w, "want args as a vector, [2]: "+err.Error(), http.StatusBadRequest)
		return
	} else if _, ok := v.(puff.Vector); !ok {
		http.Error(w, "want args as a vector, [2], got "+args, http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	fns, err := evalOk(c, "(describe "+puff.Print(puff.Keyword(addr))+")")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	mark := ""
	list, _ := plain(fns).([]any)
	for _, f := range list {
		if f, _ := f.(map[string]any); f["fn"] == fn {
			mark, _ = f["mark"].(string)
		}
	}
	switch mark {
	case "read":
	case "":
		http.Error(w, fmt.Sprintf(":%s has no function %s", addr, fn), http.StatusNotFound)
		return
	default:
		http.Error(w, fmt.Sprintf(":%s is :%s: a page fetches only :read functions", target, mark), http.StatusForbidden)
		return
	}
	resp, err := c.Message("call", target, args, s.wd)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, treeOf(resp.Value))
}

// watch streams an event, changed, each time topic is published, for a
// pinned page to fetch its view again: a connection of its own that
// follows the topic, as whiff sub does.
func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	topic, ok := keyword(r.URL.Query().Get("topic"))
	if !ok {
		http.Error(w, "want a topic", http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	resp, err := c.Sub(topic)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	fmt.Fprint(w, ": following\n\n")
	if rc.Flush() != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return
			}
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// act sends what a page picked, an item's action, [:address/fn args...],
// as you, as whiff act does, and answers its reply as a tree. Only sniff's
// own page can send it: as JSON, which another site's page can't post
// without asking first, from the same origin, and with the cookie, which
// SameSite keeps from other sites.
func (s *server) act(w http.ResponseWriter, r *http.Request) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if site := r.Header.Get("Sec-Fetch-Site"); mt != "application/json" || site != "" && site != "same-origin" {
		http.Error(w, "send it from sniff's page", http.StatusForbidden)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, `want {"action": "[:address/fn args...]"}`, http.StatusBadRequest)
		return
	}
	target, args, err := action(body.Action)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	resp, err := c.Message("call", target, args, s.wd)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, treeOf(resp.Value))
}

// action is the target and args of a, a printed [:address/fn args...].
func action(a string) (target, args string, err error) {
	v, err := puff.ReadValue(a)
	vec, ok := v.(puff.Vector)
	var k puff.Keyword
	if err == nil && ok && vec.Len() > 0 {
		k, ok = vec.At(0).(puff.Keyword)
	}
	if err != nil || !ok || vec.Len() == 0 {
		return "", "", fmt.Errorf("want [:address/fn args...], got %s", a)
	}
	rest := make([]puff.Value, vec.Len()-1)
	for i := range rest {
		rest[i] = vec.At(i + 1)
	}
	return string(k), puff.Print(puff.NewVector(rest...)), nil
}

// keyword is s without its colon, and whether it reads as a keyword: an
// address, a target or a topic.
func keyword(s string) (string, bool) {
	s = strings.TrimPrefix(s, ":")
	k, err := puff.ReadValue(":" + s)
	return s, err == nil && s != "" && puff.Equal(k, puff.Keyword(s))
}

// evalOk evaluates src, read-only code of sniff's own, and returns v of
// its (ok v).
func evalOk(c *rpc.Client, src string) (puff.Value, error) {
	resp, err := c.Eval(src, "")
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		return nil, err
	}
	v, err := puff.ReadValue(resp.Value)
	if err != nil {
		return nil, err
	}
	if t, ok := v.(puff.Tag); ok && t.Name() == "ok" && t.Len() == 1 {
		return t.At(0), nil
	}
	return nil, fmt.Errorf("%s: %s", src, resp.Value)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
