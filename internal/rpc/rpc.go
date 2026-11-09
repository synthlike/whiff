// Package rpc is how whiff's clients talk to the daemon: JSON lines over a
// per-user unix socket.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// Version is the protocol version. Bump it when a message changes, or a
// function clients evaluate is renamed (21: a submission key, which an
// older daemon would ignore and run a message twice); hello and stop must stay readable by every version, so an old
// daemon can be stopped.
const Version = 21

// Request is a client's message to the daemon. The daemon answers each with
// a Response, in order, except an answer or a cancel, which have none.
type Request struct {
	Op      string `json:"op"`                // "hello", "eval", "repl", "cancel", "call", "send", "pub", "sub", "try", "inbox", "reply", "accept", "reject", "mode", "wait", "cancel-task", "claim", "answer", "history", "trace", "ls" or "stop"
	Version int    `json:"version,omitempty"` // hello: the client's protocol
	Src     string `json:"src,omitempty"`     // eval, repl: puff code; claim: the interface, (on ...) and (ask ...) forms without bodies; try: the service file
	File    string `json:"file,omitempty"`    // try: the service file's name, for its errors
	Target  string `json:"target,omitempty"`  // call, send: address/fn; pub: the topic; inbox, claim: the address, or "agent" for an agent's next :agent/<n>; mode: the agent session's address; try: the fn
	Args    string `json:"args,omitempty"`    // call, send, pub, try: a printed puff vector, of one value for pub; reply: [id result]; wait, accept: [id]; reject: [id reason]; answer: the printed Result; sub: the topics, a printed vector of keywords, [] for every topic; mode: {:mode :gated|:trusted|:free :services [...] :for 2h}
	Dir     string `json:"dir,omitempty"`     // eval, call, send, pub, try: the client's working directory
	ID      int64  `json:"id,omitempty"`      // answer: the Delivery's ID
	// Agent says the client acts for an agent: its messages' origin is
	// :agent, not :user. eval, call, send, pub.
	Agent bool `json:"agent,omitempty"`
	// All asks history for every entry whiff.db holds, the last 30 days,
	// not only the last 1000.
	All bool `json:"all,omitempty"`
	// Token is, for a claim of "agent", an agent turn's session token,
	// from the job's WHIFF_SESSION: the claim gets the session it names,
	// fenced to that agent's :can functions.
	Token string `json:"token,omitempty"`
	// Key is, for a call or send, a submission key: a message sent again
	// with the same key, as a peer does after its connection went, gets
	// the first one's Response rather than running twice.
	Key string `json:"key,omitempty"`
	// There is, for a call or send, where it runs on a peer: whiff call
	// -C dir. Relative to the peer's dir, or to Dir for a local target.
	There string `json:"there,omitempty"`
	// Read is, for a call or send, what runs under a :read mark at the
	// sender, as a peer's view: a :write target is refused.
	Read string `json:"read,omitempty"`
}

// Response is the daemon's answer to one Request; or, with Delivery set, a
// message for the address the client holds; or, with Event set, a value
// published to a topic the client subscribed to; or, with Entry set, a new
// entry of history for a client that traces it.
type Response struct {
	Version  int       `json:"version,omitempty"` // hello: the daemon's protocol
	Value    string    `json:"value,omitempty"`   // eval: the printed value; others: the printed Result
	Out      string    `json:"out,omitempty"`     // eval: what io/println wrote
	History  []Entry   `json:"history,omitempty"` // history, trace: history so far, oldest first
	Error    string    `json:"error,omitempty"`
	Fault    bool      `json:"fault,omitempty"` // eval: Error is a fault
	Delivery *Delivery `json:"delivery,omitempty"`
	Event    *Event    `json:"event,omitempty"`
	Entry    *Entry    `json:"entry,omitempty"`
}

// Entry is one entry of history: a message and its Result, or a note about
// whiff itself, such as a service that failed to load.
type Entry struct {
	At     time.Time     `json:"at"`
	Kind   string        `json:"kind"`             // "call", "send", "pub", "done" (a [:whiff/done id result]), "eval" (an agent's that writes) or "note"
	ID     int64         `json:"id,omitempty"`     // the message's, one per entry; 0 for a note
	Cause  int64         `json:"cause,omitempty"`  // the ID of what led to it (see Chains); 0 for a message that began a chain
	Origin string        `json:"origin,omitempty"` // "user" or "agent"
	From   string        `json:"from,omitempty"`   // the sender's address; "" for a client
	To     string        `json:"to,omitempty"`     // address/fn, or address on :topic; "" for a client
	Args   string        `json:"args,omitempty"`   // a printed puff vector
	Reply  string        `json:"reply,omitempty"`  // the printed Result; "" for a done no service handled
	Took   time.Duration `json:"took,omitempty"`   // in nanoseconds
	// Gate is how the review gate treated an agent's write: "held 12" (it
	// waits for review), "refused" (it couldn't wait), "accepted 12"
	// (delivered once you accepted it), or "trusted" or "free", which
	// the session's mode let through; or "for agent/3", a service's write
	// further down a chain of agent/3's that the gate let through; "" for
	// anything else.
	Gate string `json:"gate,omitempty"`
	Note string `json:"note,omitempty"`
	// Project is the project of the chain's work, the git repository a job
	// in it ran in, as "tally"; "" until one did. What a message began
	// before its chain had one has none: see whiff history's chains.
	Project string `json:"project,omitempty"`
}

// String is e as a line of whiff history:
//
//	10:42:01.123  call  user   client -> :greet/hello "ann"  (ok "hello, ann")  1.2ms
//	10:42:02.456  eval  agent  :agent/1 -> eval "(file/write \"x\" \"y\")"  (ok :done)  [free]
func (e Entry) String() string {
	at := e.At.Local().Format("15:04:05.000")
	if e.Kind == "note" {
		return at + "  note  " + e.Note
	}
	from := "client"
	if e.From != "" {
		from = ":" + e.From
	}
	target := "client"
	switch {
	case e.Kind == "eval":
		target = "eval"
	case e.To != "":
		target = ":" + e.To
	}
	if len(e.Args) > 2 {
		target += " " + e.Args[1:len(e.Args)-1]
	}
	line := fmt.Sprintf("%s  %-4s  %-5s  %s -> %s", at, e.Kind, e.Origin, from, target)
	if e.Reply != "" {
		line += "  " + e.Reply
	}
	if e.Took > 0 {
		line += "  " + e.Took.Round(time.Microsecond).String()
	}
	if e.Gate != "" {
		line += "  [" + e.Gate + "]"
	}
	if e.Project != "" {
		line += "  #" + e.Project
	}
	return line
}

// SessionText is what the agent session addr did, from es, as Session
// finds it: a line each, then the summary.
func SessionText(es []Entry, addr string) string {
	got, summary := Session(es, addr)
	var b strings.Builder
	for _, e := range got {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String() + summary + "\n"
}

// Session is what the agent session addr did, from es, oldest first: each
// write it made, with how the review gate treated it, the writes services
// made for it further down those chains, the answers it got and the notes
// about it, such as its mode changing; and a summary:
//
//	agent/3: 3 writes went through (1 trusted, 1 free, 1 accepted), and 2 by services for it; 1 rejected; 1 waits for review
func Session(es []Entry, addr string) ([]Entry, string) {
	addr = strings.TrimPrefix(addr, ":")
	var got []Entry
	count := map[string]int{}
	held := map[string]bool{} // ids that wait, until accepted or answered
	var order []string
	for _, e := range es {
		switch {
		case e.Kind == "note" && (strings.HasPrefix(e.Note, ":"+addr+" ") || strings.HasPrefix(e.Note, ":"+addr+"'s ")):
		case e.Kind == "done" && e.To == addr+"/whiff/done":
			id, _, _ := strings.Cut(strings.TrimPrefix(e.Args, "["), " ")
			if held[id] {
				delete(held, id)
				if strings.Contains(e.Args, ":kind :rejected") {
					count["rejected"]++
				}
			}
		case e.From == addr && e.Gate != "":
			kind, id, _ := strings.Cut(e.Gate, " ")
			switch kind {
			case "held":
				held[id] = true
				order = append(order, id)
			case "accepted":
				delete(held, id)
			}
			count[kind]++
		case e.Gate == "for "+addr:
			count["for"]++
		default:
			continue
		}
		got = append(got, e)
	}
	if len(got) == 0 {
		return nil, fmt.Sprintf("%s: no writes in history", addr)
	}
	var b strings.Builder
	went := count["trusted"] + count["free"] + count["accepted"]
	var parts []string
	for _, k := range []string{"trusted", "free", "accepted"} {
		if count[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count[k], k))
		}
	}
	fmt.Fprintf(&b, "%s: %d %s went through", addr, went, plural(went, "write"))
	if len(parts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	if n := count["for"]; n > 0 {
		fmt.Fprintf(&b, ", and %d by services for it", n)
	}
	if n := count["rejected"]; n > 0 {
		fmt.Fprintf(&b, "; %d rejected", n)
	}
	if n := count["refused"]; n > 0 {
		fmt.Fprintf(&b, "; %d refused", n)
	}
	waiting := 0
	for _, id := range order {
		if held[id] {
			waiting++
		}
	}
	if waiting > 0 {
		fmt.Fprintf(&b, "; %d %s for review", waiting, plural(waiting, "waits", "wait"))
	}
	return got, b.String()
}

// plural is one, or many (one+"s" if not given), for n.
func plural(n int, one string, many ...string) string {
	switch {
	case n == 1:
		return one
	case len(many) > 0:
		return many[0]
	}
	return one + "s"
}

// HistoryText is es as whiff history prints them: newest first, a line each.
func HistoryText(es []Entry) string {
	var b strings.Builder
	for i := len(es) - 1; i >= 0; i-- {
		b.WriteString(es[i].String())
		b.WriteByte('\n')
	}
	return b.String()
}

// Chained is an entry of history in its chain: Depth is 0 for the one that
// began it, 1 for what that led to, and so on.
type Chained struct {
	Entry
	Depth int
}

// Chains is es, oldest first, as chains: each entry that began one, newest
// first, then what it led to, as each began, below what led to it. An
// entry whose cause isn't in es begins a chain of its own.
func Chains(es []Entry) []Chained {
	ids := map[int64]bool{}
	for _, e := range es {
		if e.ID != 0 {
			ids[e.ID] = true
		}
	}
	var roots []Entry
	led := map[int64][]Entry{}
	for _, e := range es {
		if ids[e.Cause] {
			led[e.Cause] = append(led[e.Cause], e)
		} else {
			roots = append(roots, e)
		}
	}
	var got []Chained
	var add func(e Entry, depth int)
	add = func(e Entry, depth int) {
		got = append(got, Chained{e, depth})
		next := led[e.ID]
		slices.SortStableFunc(next, func(a, b Entry) int { return a.At.Compare(b.At) })
		for _, n := range next {
			add(n, depth+1)
		}
	}
	for _, e := range slices.Backward(roots) {
		add(e, 0)
	}
	return got
}

// ChainsText is es as whiff history -c prints them: as Chains, a line each,
// indented by depth after the time.
//
//	10:42:01.123  call  user   client -> :plan/draft "x"  (ok :done)  3ms
//	10:42:01.124    call  user   :plan -> :git/status  (ok "clean")  1ms
func ChainsText(es []Entry) string {
	var b strings.Builder
	for _, c := range Chains(es) {
		line := c.String()
		at, rest, _ := strings.Cut(line, "  ")
		b.WriteString(at + "  " + strings.Repeat("  ", c.Depth) + rest)
		b.WriteByte('\n')
	}
	return b.String()
}

// Event is a value published to a topic a client subscribed to.
type Event struct {
	At     time.Time `json:"at"`
	Topic  string    `json:"topic"`
	Value  string    `json:"value"`          // the printed value
	From   string    `json:"from,omitempty"` // the publisher's address; "" for a client
	Origin string    `json:"origin"`         // "user" or "agent"
}

// Delivery is a message to a function of the address a client holds. The
// client answers it with an answer Request carrying its ID and Result,
// within the caller's timeout.
type Delivery struct {
	ID     int64  `json:"id"`
	Fn     string `json:"fn"`
	Args   string `json:"args"`           // a printed puff vector
	From   string `json:"from,omitempty"` // the sender's address; "" for a client
	Origin string `json:"origin"`         // "user" or "agent"
	Dir    string `json:"dir,omitempty"`  // the sender's working directory
}

// Conn sends and receives JSON lines, over a unix socket or a command's
// stdin and stdout (DialCommand). Send is safe for concurrent use.
type Conn struct {
	c   io.ReadWriteCloser
	mu  sync.Mutex // held while sending
	enc *json.Encoder
	dec *json.Decoder
}

func NewConn(c io.ReadWriteCloser) *Conn {
	return &Conn{c: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
}

func (c *Conn) Send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(v)
}
func (c *Conn) Recv(v any) error { return c.dec.Decode(v) }
func (c *Conn) Close() error     { return c.c.Close() }

// DefaultDir is the per-user directory holding the socket.
func DefaultDir() string {
	return filepath.Join("/tmp", fmt.Sprintf("whiff-%d", os.Getuid()))
}

// SocketPath is the daemon's socket in dir.
func SocketPath(dir string) string { return filepath.Join(dir, "whiff.sock") }

// PrepareDir creates dir with mode 0700 if it doesn't exist, and checks that
// it is a directory (not a link) owned by this user that nobody else can
// open, so no one else can listen in place of the daemon.
func PrepareDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s is owned by another user", dir)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is open to other users (mode %v), want 0700", dir, fi.Mode().Perm())
	}
	return nil
}

// Client is a connection to the daemon after the hello. Its methods are
// safe for concurrent use; requests go one at a time.
type Client struct {
	conn *Conn
	// Daemon is the daemon's protocol version.
	Daemon int
	// Agent makes every request act for an agent: see Request.Agent.
	Agent bool

	mu         sync.Mutex // held for a request and its response
	resps      chan Response
	deliveries chan Delivery
	events     chan Event    // closed when the connection ends
	entries    chan Entry    // closed when the connection ends
	done       chan struct{} // closed when the connection ends
	err        error         // why it ended, set before done is closed
}

// Dial connects to the daemon in dir and exchanges protocol versions. If no
// daemon answers and start isn't nil, it calls start and waits up to 2s for
// the daemon to listen.
func Dial(dir string, start func() error) (*Client, error) {
	if err := PrepareDir(dir); err != nil {
		return nil, err
	}
	path := SocketPath(dir)
	nc, err := net.Dial("unix", path)
	if err != nil && start != nil {
		if err := start(); err != nil {
			return nil, fmt.Errorf("starting whiff: %w", err)
		}
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			nc, err = net.Dial("unix", path)
			if err == nil || time.Now().After(deadline) {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return hello(nc)
}

// hello is a Client over rwc, once the daemon at its other end has said
// which protocol it speaks.
func hello(rwc io.ReadWriteCloser) (*Client, error) {
	c := &Client{conn: NewConn(rwc), resps: make(chan Response),
		deliveries: make(chan Delivery, 100), events: make(chan Event, 100), entries: make(chan Entry, 100),
		done: make(chan struct{})}
	go c.read()
	var resp Response
	if err := c.roundTrip(Request{Op: "hello", Version: Version}, &resp); err != nil {
		c.Close()
		return nil, err
	}
	c.Daemon = resp.Version
	return c, nil
}

// DialCommand runs argv, with env added to whiff's own, as a daemon at the
// other end of its stdin and stdout, and exchanges protocol versions: how
// a peer is reached, by ssh host whiff bridge, which pipes them to that
// host's daemon. If the command ends before the daemon answers, as ssh
// does when it can't log in, the error is the last line it printed on
// stderr, which says what it was, as ssh's do. Closing the Client closes its stdin, so the command can end
// as the bridge does, and waits for it, killing it after waitGrace.
func DialCommand(ctx context.Context, argv, env []string) (*Client, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to reach the daemon by")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = waitGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c, err := hello(&cmdConn{cmd: cmd, r: stdout, w: stdin})
	if err != nil {
		if said := stderr.String(); said != "" {
			return nil, errors.New(said)
		}
		return nil, fmt.Errorf("%s: %v", strings.Join(argv, " "), err)
	}
	return c, nil
}

// waitGrace is how long a closed command has to end before it is killed.
const waitGrace = 2 * time.Second

// cmdConn is a command's stdout and stdin as one connection.
type cmdConn struct {
	cmd  *exec.Cmd
	r    io.Reader
	w    io.WriteCloser
	once sync.Once
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close closes the command's stdin and waits for it to end.
func (c *cmdConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		go func() {
			time.Sleep(waitGrace)
			c.cmd.Process.Kill()
		}()
		c.cmd.Wait()
	})
	return nil
}

// tailBuffer keeps the last 2KiB written to it, for a command's stderr.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if n := len(b.buf) - 2048; n > 0 {
		b.buf = b.buf[n:]
	}
	return len(p), nil
}

// String is the last line written, trimmed.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(string(b.buf))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// Compatible says what to do when the daemon speaks another protocol. name
// is the client's, as you start it: a long-running one (sniff, whiff mcp)
// may be older than the binary installed since.
func (c *Client) Compatible(name string) error {
	switch {
	case c.Daemon < Version:
		return fmt.Errorf("the daemon speaks protocol %d, this %s %d: run `whiff stop` and try again", c.Daemon, name, Version)
	case c.Daemon > Version:
		return fmt.Errorf("the daemon speaks protocol %d, this %s only %d: reinstall whiff and start %s again", c.Daemon, name, Version, name)
	}
	return nil
}

// PeerCompatible says what to do when peer, a daemon reached by
// DialCommand, speaks another protocol than this whiff.
func (c *Client) PeerCompatible(peer string) error {
	if c.Daemon != Version {
		return fmt.Errorf("%s speaks protocol %d, this whiff %d: install the same whiff on both", peer, c.Daemon, Version)
	}
	return nil
}

// Eval evaluates src in the daemon, with dir as its working directory.
func (c *Client) Eval(src, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "eval", Src: src, Dir: dir}, &resp)
	return resp, err
}

// EvalValue evaluates src like Eval and returns its value, read, for code
// that prints nothing: an error the evaluation ends in is its error.
func (c *Client) EvalValue(src, dir string) (puff.Value, error) {
	resp, err := c.Eval(src, dir)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		return nil, err
	}
	return puff.ReadValue(resp.Value)
}

// Repl evaluates src like Eval, but in this connection's own env, which
// keeps what earlier Repl calls defined.
func (c *Client) Repl(src, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "repl", Src: src, Dir: dir}, &resp)
	return resp, err
}

// Cancel stops the Eval or Repl this connection is waiting for, which then
// returns its error; it does nothing if there is none.
func (c *Client) Cancel() error { return c.conn.Send(Request{Op: "cancel"}) }

// Message calls or sends (op) target, address/fn, with args, a printed puff
// vector, from dir; publishes args's one value to the topic target (op
// "pub"); answers a message in an inbox, with args [id result] (op
// "reply"); or accepts or rejects a message that waits for review, with
// args [id] (op "accept") or [id reason] (op "reject"); or cancels a task
// that hasn't ended, with args [id] (op "cancel-task"). The Response's
// Value is the printed Result.
func (c *Client) Message(op, target, args, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: op, Target: target, Args: args, Dir: dir}, &resp)
	return resp, err
}

// Try loads src, the service file named file, under a scratch address,
// calls its function fn with args, a printed puff vector, from dir, and
// unloads it. The Response's Value is the printed Result; a file that
// doesn't load is its Error, at file:line:col.
func (c *Client) Try(file, src, fn, args, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "try", File: file, Src: src, Target: fn, Args: args, Dir: dir}, &resp)
	return resp, err
}

// Test loads src, the service file named file, under a scratch address
// without starting it, runs its tests from dir and unloads it. The
// Response's Value is a printed vector, a {:test :at :message} for each
// test, with :message "" if it passed; a file that doesn't load is its
// Error, at file:line:col.
func (c *Client) Test(file, src, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "test", File: file, Src: src, Dir: dir}, &resp)
	return resp, err
}

// Mode sets the review gate's mode for the agent session at addr: mode is
// "gated", "trusted" (with services, the ones whose messages go through)
// or "free"; for, if not 0, ends it after that long. The Response's Value
// is (ok :done), or (err {:kind :no-address}).
func (c *Client) Mode(addr, mode string, services []string, d time.Duration) (Response, error) {
	args := "{:mode :" + mode
	if len(services) > 0 {
		keys := make([]string, len(services))
		for i, s := range services {
			keys[i] = ":" + strings.TrimPrefix(s, ":")
		}
		args += " :services [" + strings.Join(keys, " ") + "]"
	}
	if d > 0 {
		args += fmt.Sprintf(" :for %ds", int64(d/time.Second))
	}
	var resp Response
	err := c.roundTrip(Request{Op: "mode", Target: addr, Args: args + "}"}, &resp)
	return resp, err
}

// History is the daemon's history, oldest first.
func (c *Client) History() ([]Entry, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "history"}, &resp)
	return resp.History, err
}

// AllHistory is History with every entry whiff.db holds, the last 30
// days, not only the last 1000.
func (c *Client) AllHistory() ([]Entry, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "history", All: true}, &resp)
	return resp.History, err
}

// Trace is History, and from then on every new entry comes on Entries until
// the connection closes, with none missed or repeated in between.
func (c *Client) Trace() ([]Entry, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "trace"}, &resp)
	return resp.History, err
}

// Entries are the new entries of history after Trace; the channel closes
// when the connection ends. A client that doesn't keep reading them misses
// some: the daemon doesn't wait for it.
func (c *Client) Entries() <-chan Entry { return c.entries }

// Inbox is the messages waiting in addr's inbox, oldest first, a line each.
func (c *Client) Inbox(addr string) (string, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "inbox", Target: addr}, &resp)
	return resp.Out, err
}

// Ls is every address's functions and the warnings about them, as text.
func (c *Client) Ls() (string, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "ls"}, &resp)
	return resp.Out, err
}

// Claim makes addr this connection's address, with iface, its (on ...) and
// (ask ...) forms without bodies. The Response's Value is (ok :done), or
// (err {:kind :taken}) if addr is held or whiff's own. An Agent client that
// claims "agent" gets the next :agent/<n> instead, as (ok :agent/<n>).
// Messages to addr then come on Deliveries until the connection closes.
func (c *Client) Claim(addr, iface string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "claim", Target: addr, Src: iface}, &resp)
	return resp, err
}

// ClaimSession claims the agent session token names, for an agent's turn,
// with the interface in iface: its Value is (ok [:agent/<name> [fns...]]),
// the session and the functions it may call.
func (c *Client) ClaimSession(token, iface string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "claim", Target: "agent", Src: iface, Token: token}, &resp)
	return resp, err
}

// Sub subscribes this connection to topics, every topic if there are none:
// what is published to them then comes on Events until the connection
// closes.
func (c *Client) Sub(topics ...string) (Response, error) {
	keys := make([]string, len(topics))
	for i, t := range topics {
		keys[i] = ":" + strings.TrimPrefix(t, ":")
	}
	var resp Response
	err := c.roundTrip(Request{Op: "sub", Args: "[" + strings.Join(keys, " ") + "]"}, &resp)
	return resp, err
}

// Events are the values published to the topics this connection subscribed
// to; the channel closes when the connection ends. A client that doesn't
// keep reading them misses some: the daemon doesn't wait for it.
func (c *Client) Events() <-chan Event { return c.events }

// Deliveries are the messages to the address this connection holds. A
// client that claims one keeps reading them: until it does, the responses
// to its requests wait behind them.
func (c *Client) Deliveries() <-chan Delivery { return c.deliveries }

// Answer answers the delivery id with result, a printed (ok v) or (err e).
func (c *Client) Answer(id int64, result string) error {
	return c.conn.Send(Request{Op: "answer", ID: id, Args: result})
}

// Stop asks the daemon to stop and waits until it has stopped listening.
func (c *Client) Stop() error {
	var resp Response
	if err := c.roundTrip(Request{Op: "stop"}, &resp); err != nil {
		return err
	}
	// The daemon closes every connection once it no longer listens.
	<-c.done
	return nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// read sorts what the daemon sends into responses and deliveries until the
// connection ends.
func (c *Client) read() {
	defer close(c.events)
	defer close(c.entries)
	defer close(c.done)
	for {
		var r Response
		if err := c.conn.Recv(&r); err != nil {
			c.err = err
			return
		}
		if r.Delivery != nil {
			c.deliveries <- *r.Delivery
			continue
		}
		if r.Event != nil {
			select {
			case c.events <- *r.Event:
			default: // nobody reads them
			}
			continue
		}
		if r.Entry != nil {
			select {
			case c.entries <- *r.Entry:
			default:
			}
			continue
		}
		c.resps <- r
	}
}

// Forward sends req as it is, as an agent's if it or c says so, and
// returns its Response, with the daemon's error in its Error: the error is
// for the connection, which has gone.
func (c *Client) Forward(req Request) (Response, error) {
	var resp Response
	c.mu.Lock()
	defer c.mu.Unlock()
	req.Agent = req.Agent || c.Agent
	if err := c.conn.Send(req); err != nil {
		return resp, err
	}
	select {
	case resp = <-c.resps:
		return resp, nil
	case <-c.done:
		return resp, fmt.Errorf("whiff daemon: %w", c.err)
	}
}

func (c *Client) roundTrip(req Request, resp *Response) error {
	var err error
	if *resp, err = c.Forward(req); err != nil {
		return err
	}
	if resp.Error != "" && req.Op != "eval" && req.Op != "repl" && req.Op != "try" && req.Op != "test" {
		return errors.New(resp.Error)
	}
	return nil
}
