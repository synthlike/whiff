// Package rpc is how whiff's clients talk to the daemon: JSON lines over a
// per-user unix socket.
package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Version is the protocol version. Bump it when a message changes; hello and
// stop must stay readable by every version, so an old daemon can be stopped.
const Version = 9

// Request is a client's message to the daemon. The daemon answers each with
// a Response, in order, except an answer, which has none.
type Request struct {
	Op      string `json:"op"`                // "hello", "eval", "call", "send", "pub", "try", "inbox", "reply", "wait", "claim", "answer", "history", "ls" or "stop"
	Version int    `json:"version,omitempty"` // hello: the client's protocol
	Src     string `json:"src,omitempty"`     // eval: puff code; claim: the interface, (on ...) and (ask ...) forms without bodies; try: the service file
	File    string `json:"file,omitempty"`    // try: the service file's name, for its errors
	Target  string `json:"target,omitempty"`  // call, send: address/fn; pub: the topic; inbox, claim: the address; try: the fn
	Args    string `json:"args,omitempty"`    // call, send, pub, try: a printed puff vector, of one value for pub; reply: [id result]; wait: [id]; answer: the printed Result
	Dir     string `json:"dir,omitempty"`     // eval, call, send, pub, try: the client's working directory
	ID      int64  `json:"id,omitempty"`      // answer: the Delivery's ID
	// Agent says the client acts for an agent: its messages' origin is
	// :agent, not :user. eval, call, send, pub.
	Agent bool `json:"agent,omitempty"`
}

// Response is the daemon's answer to one Request, or, with Delivery set, a
// message for the address the client holds.
type Response struct {
	Version  int       `json:"version,omitempty"` // hello: the daemon's protocol
	Value    string    `json:"value,omitempty"`   // eval: the printed value; others: the printed Result
	Out      string    `json:"out,omitempty"`     // eval: what io/println wrote
	Error    string    `json:"error,omitempty"`
	Fault    bool      `json:"fault,omitempty"` // eval: Error is a fault
	Delivery *Delivery `json:"delivery,omitempty"`
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

// Conn sends and receives JSON lines. Send is safe for concurrent use.
type Conn struct {
	c   net.Conn
	mu  sync.Mutex // held while sending
	enc *json.Encoder
	dec *json.Decoder
}

func NewConn(c net.Conn) *Conn {
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
	c := &Client{conn: NewConn(nc), resps: make(chan Response),
		deliveries: make(chan Delivery, 100), done: make(chan struct{})}
	go c.read()
	var resp Response
	if err := c.roundTrip(Request{Op: "hello", Version: Version}, &resp); err != nil {
		c.Close()
		return nil, err
	}
	c.Daemon = resp.Version
	return c, nil
}

// Compatible says what to do when the daemon speaks another protocol.
func (c *Client) Compatible() error {
	switch {
	case c.Daemon < Version:
		return fmt.Errorf("the daemon speaks protocol %d, this whiff %d: run `whiff stop` and try again", c.Daemon, Version)
	case c.Daemon > Version:
		return fmt.Errorf("the daemon speaks protocol %d, this whiff only %d: reinstall whiff", c.Daemon, Version)
	}
	return nil
}

// Eval evaluates src in the daemon, with dir as its working directory.
func (c *Client) Eval(src, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "eval", Src: src, Dir: dir}, &resp)
	return resp, err
}

// Message calls or sends (op) target, address/fn, with args, a printed puff
// vector, from dir; publishes args's one value to the topic target (op
// "pub"); or answers a message in an inbox, with args [id result] (op
// "reply"). The Response's Value is the printed Result.
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

// History is the daemon's history, newest first, a line an entry.
func (c *Client) History() (string, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "history"}, &resp)
	return resp.Out, err
}

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
// (err {:kind :taken}) if addr is held or whiff's own. Messages to addr then
// come on Deliveries until the connection closes.
func (c *Client) Claim(addr, iface string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "claim", Target: addr, Src: iface}, &resp)
	return resp, err
}

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

// read sorts what the daemon sends into responses and deliveries until the
// connection ends.
func (c *Client) read() {
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
		c.resps <- r
	}
}

func (c *Client) roundTrip(req Request, resp *Response) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	req.Agent = c.Agent
	if err := c.conn.Send(req); err != nil {
		return err
	}
	select {
	case *resp = <-c.resps:
	case <-c.done:
		return fmt.Errorf("whiff daemon: %w", c.err)
	}
	if resp.Error != "" && req.Op != "eval" && req.Op != "try" {
		return errors.New(resp.Error)
	}
	return nil
}
