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
	"syscall"
	"time"
)

// Version is the protocol version. Bump it when a message changes; hello and
// stop must stay readable by every version, so an old daemon can be stopped.
const Version = 3

// Request is a client's message to the daemon.
type Request struct {
	Op      string `json:"op"`                // "hello", "eval", "call", "send", "history", "ls" or "stop"
	Version int    `json:"version,omitempty"` // hello: the client's protocol
	Src     string `json:"src,omitempty"`     // eval: puff code
	Target  string `json:"target,omitempty"`  // call, send: address/fn
	Args    string `json:"args,omitempty"`    // call, send: a printed puff vector
	Dir     string `json:"dir,omitempty"`     // eval, call, send: the client's working directory
}

// Response is the daemon's answer to one Request.
type Response struct {
	Version int    `json:"version,omitempty"` // hello: the daemon's protocol
	Value   string `json:"value,omitempty"`   // eval: the printed value
	Out     string `json:"out,omitempty"`     // eval: what io/println wrote
	Error   string `json:"error,omitempty"`
	Fault   bool   `json:"fault,omitempty"` // eval: Error is a fault
}

// Conn sends and receives JSON lines.
type Conn struct {
	c   net.Conn
	enc *json.Encoder
	dec *json.Decoder
}

func NewConn(c net.Conn) *Conn {
	return &Conn{c: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
}

func (c *Conn) Send(v any) error { return c.enc.Encode(v) }
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

// Client is a connection to the daemon after the hello.
type Client struct {
	conn *Conn
	// Daemon is the daemon's protocol version.
	Daemon int
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
	c := &Client{conn: NewConn(nc)}
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
// vector, from dir. The Response's Value is the printed Result.
func (c *Client) Message(op, target, args, dir string) (Response, error) {
	var resp Response
	err := c.roundTrip(Request{Op: op, Target: target, Args: args, Dir: dir}, &resp)
	return resp, err
}

// History is the daemon's history, newest first, a line an entry.
func (c *Client) History() (string, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "history"}, &resp)
	return resp.Out, err
}

// Ls is every address's functions and the warnings about them, as text.
func (c *Client) Ls() (string, error) {
	var resp Response
	err := c.roundTrip(Request{Op: "ls"}, &resp)
	return resp.Out, err
}

// Stop asks the daemon to stop and waits until it has stopped listening.
func (c *Client) Stop() error {
	var resp Response
	if err := c.roundTrip(Request{Op: "stop"}, &resp); err != nil {
		return err
	}
	// The daemon closes every connection once it no longer listens.
	c.conn.Recv(&resp)
	return nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) roundTrip(req Request, resp *Response) error {
	if err := c.conn.Send(req); err != nil {
		return err
	}
	if err := c.conn.Recv(resp); err != nil {
		return fmt.Errorf("whiff daemon: %w", err)
	}
	if resp.Error != "" && req.Op != "eval" {
		return errors.New(resp.Error)
	}
	return nil
}
