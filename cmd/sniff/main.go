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
// /view/address/fn?args=[...], drawn here (render.go) and fetched again
// each time the topic it names fires ({:changed topic}, ?on=topic, or
// address/changed). An item's action to a :read function is a link to its
// page; any other is a form that sends it, as whiff act does.
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
// traces it, which it pushes to every page open, each only what its query
// matches.
type server struct {
	dir   string // the daemon's socket directory
	wd    string // the directory sniff calls from, as the CLI calls from its own
	port  int
	token string

	mu       sync.Mutex
	entries  []stored // history, oldest first
	status   status
	watchers map[chan event]watcher
	marks    map[string]string // every function's mark by target, for queries with mark:
	marksAt  time.Time
	said     map[int]said // what the last actions sent from a page answered, by number
	saidN    int
}

// stored is an entry of history, and the entry as pages get it.
type stored struct {
	e    rpc.Entry
	json json.RawMessage
}

// watcher is what a page open follows: the entries its query, ?q=,
// matches; none if its query has an error, which its snapshot says.
type watcher struct {
	q   rpc.Query
	err string
}

// said is what an action a page sent answered, for the page it goes back
// to to show.
type said struct {
	Sends, Reply string
	OK           bool
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
		status: status{Why: "connecting to whiff"}, watchers: map[chan event]watcher{}, said: map[int]said{}}
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
		s.entries = append(s.entries, stored{e, entryJSON(e)})
	}
	s.status = status{Connected: true}
	s.mu.Unlock()
	ms := s.marksFor(nil, false)
	s.mu.Lock()
	for ch, w := range s.watchers {
		s.push(ch, event{"snapshot", s.snapshot(w, ms)})
	}
	s.mu.Unlock()
	for {
		select {
		case e, ok := <-c.Entries():
			if !ok {
				return errors.New("whiff stopped")
			}
			s.add(e)
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

// add keeps e, and sends it to every page whose query it matches.
func (s *server) add(e rpc.Entry) {
	st := stored{e, entryJSON(e)}
	ms := s.marksFor(&e, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, st)
	if n := len(s.entries) - historyKeep; n > 0 {
		s.entries = slices.Delete(s.entries, 0, n)
	}
	for ch, w := range s.watchers {
		if w.err == "" && w.q.Match(e, ms) {
			s.push(ch, event{"entry", st.json})
		}
	}
}

// marksFor is every function's mark, if a page's query asks for it with
// mark: (asked, for one about to open), else nil. They are fetched again
// when e is to a function they don't know, at most every two seconds; e
// is nil for a snapshot.
func (s *server) marksFor(e *rpc.Entry, asked bool) map[string]string {
	s.mu.Lock()
	for _, w := range s.watchers {
		asked = asked || w.q.Marks()
	}
	ms, at := s.marks, s.marksAt
	s.mu.Unlock()
	if !asked {
		return nil
	}
	known := ms != nil && (e == nil || (e.Kind != "call" && e.Kind != "send") || ms[e.To] != "")
	if known || time.Since(at) < 2*time.Second {
		return ms
	}
	c, err := s.dial()
	if err != nil {
		return ms
	}
	defer c.Close()
	fresh, err := c.Marks()
	if err != nil {
		return ms
	}
	s.mu.Lock()
	s.marks, s.marksAt = fresh, time.Now()
	s.mu.Unlock()
	return fresh
}

func (s *server) setStatus(st status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
	data, _ := json.Marshal(st)
	s.broadcast(event{"status", data})
}

// snapshot is what a page that follows w gets when it opens, and when
// sniff follows the daemon again: {:status :entries}, the entries w's
// query matches, with :error if it has one. Its caller holds s.mu.
func (s *server) snapshot(w watcher, marks map[string]string) []byte {
	out := struct {
		Status  status            `json:"status"`
		Entries []json.RawMessage `json:"entries"`
		Error   string            `json:"error,omitempty"`
	}{Status: s.status, Entries: []json.RawMessage{}, Error: w.err}
	if w.err == "" {
		for _, st := range s.entries {
			if w.q.Match(st.e, marks) {
				out.Entries = append(out.Entries, st.json)
			}
		}
	}
	data, _ := json.Marshal(out)
	return data
}

// broadcast sends e to every page.
func (s *server) broadcast(e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.watchers {
		s.push(ch, e)
	}
}

// push sends e to the page ch is for. One that doesn't keep up is closed:
// its browser opens it again and gets a fresh snapshot. Its caller holds
// s.mu.
func (s *server) push(ch chan event, e event) {
	select {
	case ch <- e:
	default:
		delete(s.watchers, ch)
		close(ch)
	}
}

// handler is sniff's routes, behind its checks.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(web, "web")
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /view/", s.page)
	mux.HandleFunc("POST /act", s.act)
	mux.HandleFunc("POST /render", s.render)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /api/addresses", s.addresses)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/view", s.view)
	mux.HandleFunc("GET /api/watch", s.watch)
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
		h.Set("Referrer-Policy", "same-origin")
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
// and statuses as they come; only the entries its query, ?q=, matches, as
// whiff history's (rpc.Query).
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	q, err := rpc.ParseQuery(r.URL.Query().Get("q"))
	wt := watcher{q: q}
	if err != nil {
		wt.err = err.Error()
	}
	ms := s.marksFor(nil, q.Marks())
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	ch := make(chan event, 256)
	// Following from the snapshot on, under one lock, no entry is missed
	// or sent twice.
	s.mu.Lock()
	s.watchers[ch] = wt
	first := s.snapshot(wt, ms)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
		s.mu.Unlock()
	}()
	send := func(e event) bool {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		return rc.Flush() == nil
	}
	if !send(event{"snapshot", first}) {
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
	out, err := describeAll(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, out)
}

// describeAll is every address with its functions, as addresses answers.
func describeAll(c *rpc.Client) ([]any, error) {
	addrs, err := evalOk(c, "(address/list)")
	if err != nil {
		return nil, err
	}
	out := []any{}
	vec, _ := addrs.(puff.Vector)
	for i := range vec.Len() {
		a, _ := plain(vec.At(i)).(map[string]any)
		name, _ := a["address"].(string)
		fns, err := evalOk(c, "(address/describe "+puff.Print(puff.Keyword(name))+")")
		if err != nil {
			continue // gone since
		}
		a["fns"] = plain(fns)
		out = append(out, a)
	}
	return out, nil
}

// state answers the address's state, (service/state :address)'s Result as a
// tree.
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
	resp, err := c.Eval("(service/state "+puff.Print(puff.Keyword(name))+")", "")
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
	reply, _, code, err := s.fetch(r.URL.Query().Get("target"), r.URL.Query().Get("args"))
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, treeOf(reply))
}

// fetch is what target, address/fn, answers called as you with args, a
// printed vector ("" for none), if it is a :read function: its reply,
// printed, and every function's mark by target; or why not, with the HTTP
// status that says so.
func (s *server) fetch(target, args string) (string, map[string]string, int, error) {
	target, ok := keyword(target)
	i := strings.LastIndex(target, "/")
	if !ok || i <= 0 {
		return "", nil, http.StatusBadRequest, errors.New("want a target, address/fn")
	}
	if args == "" {
		args = "[]"
	}
	if v, err := puff.ReadValue(args); err != nil {
		return "", nil, http.StatusBadRequest, errors.New("want args as a vector, [2]: " + err.Error())
	} else if _, ok := v.(puff.Vector); !ok {
		return "", nil, http.StatusBadRequest, errors.New("want args as a vector, [2], got " + args)
	}
	c, err := s.dial()
	if err != nil {
		return "", nil, http.StatusServiceUnavailable, err
	}
	defer c.Close()
	ms, err := c.Marks()
	if err != nil {
		return "", nil, http.StatusBadGateway, err
	}
	switch mark := ms[target]; mark {
	case "read":
	case "":
		return "", nil, http.StatusNotFound, fmt.Errorf(":%s has no function %s", target[:i], target[i+1:])
	default:
		return "", nil, http.StatusForbidden, fmt.Errorf(":%s is :%s: a page fetches only :read functions", target, mark)
	}
	resp, err := c.Message("call", target, args, s.wd)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		return "", nil, http.StatusBadGateway, err
	}
	return resp.Value, ms, http.StatusOK, nil
}

// page draws a :read function's view as a page of its own,
// /view/address/fn?args=[...], fetched again each time one of its topics
// fires: ?on=topic, as often as needed, or the ones the view names, or
// address/changed. ?part=view draws only the view, for the page to swap
// in, ?part=panel the view with its topics, for a tab of the main page, and
// ?part=first only an items view's first item, and a link here for the rest,
// for its review banner; ?said=n shows what the action sent from it
// answered.
func (s *server) page(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target := strings.TrimPrefix(r.URL.Path, "/view/")
	args := q.Get("args")
	title := ":" + target
	if args != "" && args != "[]" {
		title += " " + strings.TrimSuffix(strings.TrimPrefix(args, "["), "]")
	}
	// Back is the page itself, without what only this request asks for.
	part := q.Get("part")
	n, _ := strconv.Atoi(q.Get("said"))
	q.Del("part")
	q.Del("said")
	back := r.URL.Path
	if len(q) > 0 {
		back += "?" + q.Encode()
	}
	data := struct {
		Shown, On, Topics, Error string
		Page                     string // the page itself
		Reply                    shown
		Said                     *said
	}{Shown: title, Page: back}
	reply, ms, code, err := s.fetch(target, args)
	if err != nil {
		if part != "" {
			http.Error(w, err.Error(), code)
			return
		}
		data.Error = err.Error()
	} else {
		data.Reply = show(reply, ms, back)
		topics := q["on"]
		if len(topics) == 0 && data.Reply.View != nil {
			topics = data.Reply.View.Topics
		}
		if len(topics) == 0 {
			topics = []string{target[:max(strings.LastIndex(target, "/"), 0)] + "/changed"}
		}
		data.Topics = strings.Join(topics, " ")
		data.On = ":" + strings.Join(topics, ", :")
	}
	s.mu.Lock()
	if sd, ok := s.said[n]; ok {
		data.Said = &sd
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch part {
	case "":
	case "panel", "first":
		pages.ExecuteTemplate(w, part, data)
		return
	default:
		pages.ExecuteTemplate(w, "shown", data.Reply)
		return
	}
	if err != nil {
		w.WriteHeader(code)
	}
	pages.ExecuteTemplate(w, "page", data)
}

// watch streams an event, changed, each time one of its topics is
// published, ?topic=a&topic=b, for a pinned page to fetch its view again: a
// connection of its own that follows them, as whiff sub does.
func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	var topics []string
	for _, t := range r.URL.Query()["topic"] {
		topic, ok := keyword(t)
		if !ok {
			http.Error(w, "want a topic, got "+t, http.StatusBadRequest)
			return
		}
		topics = append(topics, topic)
	}
	if len(topics) == 0 {
		http.Error(w, "want a topic", http.StatusBadRequest)
		return
	}
	c, err := s.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()
	resp, err := c.Sub(topics...)
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

// render draws what the main page posts, a printed value, as sniff draws
// views: ?as=reply as a reply, its view with only its links, since a past
// reply's actions may no longer hold (its page is where to act), and the
// reply folded under it; otherwise the value folded.
func (s *server) render(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.URL.Query().Get("as") == "reply" {
		ms := map[string]string{}
		if c, err := s.dial(); err == nil {
			if m, err := c.Marks(); err == nil {
				ms = m
			}
			c.Close()
		}
		pages.ExecuteTemplate(w, "shown", show(string(b), ms, ""))
		return
	}
	n := node{Kind: "lit", Text: string(b)}
	if v, err := puff.ReadValue(string(b)); err == nil {
		n = fold(v, 0)
	}
	pages.ExecuteTemplate(w, "value", n)
}

// act sends what a page's form picked, an item's action, as you, as whiff
// act does, with your answer added if the form asked for one, and goes
// back to the page it came from, which shows what it answered. Only
// sniff's own pages can post it: the cookie is SameSite, and a browser
// says where a post comes from, with Sec-Fetch-Site, which no page can
// set; or, an older one, with Origin.
func (s *server) act(w http.ResponseWriter, r *http.Request) {
	site, o := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" {
		http.Error(w, fmt.Sprintf("send it from sniff's page, not from a %s one", site), http.StatusForbidden)
		return
	}
	if site == "" && o != "" && o != fmt.Sprintf("http://127.0.0.1:%d", s.port) && o != fmt.Sprintf("http://localhost:%d", s.port) {
		http.Error(w, fmt.Sprintf("send it from sniff's page, not from %s", o), http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := r.PostForm.Get("action")
	if answer, ok := r.PostForm["answer"]; ok {
		v, err := puff.ReadValue(msg)
		if vec, ok := v.(puff.Vector); err == nil && ok {
			items := make([]puff.Value, vec.Len(), vec.Len()+1)
			for i := range items {
				items[i] = vec.At(i)
			}
			msg = puff.Print(puff.NewVector(append(items, puff.String(strings.TrimSpace(strings.Join(answer, ""))))...))
		}
	}
	target, args, err := action(msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// back is a page of sniff's own, never another site.
	back := r.PostForm.Get("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, "/\\") {
		back = "/"
	}
	sd := said{Sends: msg}
	if c, err := s.dial(); err != nil {
		sd.Reply = err.Error()
	} else {
		resp, err := c.Message("call", target, args, s.wd)
		c.Close()
		switch {
		case err != nil:
			sd.Reply = err.Error()
		case resp.Error != "":
			sd.Reply = resp.Error
		default:
			sd.Reply = resp.Value
			sd.OK = strings.HasPrefix(resp.Value, "(ok ")
		}
	}
	s.mu.Lock()
	s.saidN++
	n := s.saidN
	s.said[n] = sd
	delete(s.said, n-64)
	s.mu.Unlock()
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, fmt.Sprintf("%s%ssaid=%d", back, sep, n), http.StatusSeeOther)
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
