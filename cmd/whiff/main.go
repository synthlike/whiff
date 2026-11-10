// Command whiff is the whiff daemon and its clients.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/synthlike/whiff/internal/daemon"
	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

const usage = `usage:
  whiff call [-C <dir>] <address/fn> [<arg>...]
                                         call a function and print its value, broken over lines to fit on a terminal;
                                         address@peer is a peer's, run in its dir, or <dir> there
  whiff send [-C <dir>] <address/fn> [<arg>...]
                                         send a message without waiting
  whiff pub <topic> <value>              publish value to the topic's subscribers
  whiff sub [<topic>...]                 print what is published to the topics, every topic by default, until stopped
  whiff try <file.puff> <fn> [<arg>...]  load a service file, call its function once and unload it
  whiff test <file.puff>...              run the tests in service files, without starting them
  whiff act [<line>...]                  send the action at the end of each line, an item as call prints it, or of stdin's
  whiff inbox [<address>]                list the messages waiting for a reply: yours, :me's, by default, and each peer's
                                         as 12@vps; address@peer for only that peer's
  whiff reply <id> <value>               answer a message waiting for a reply; a peer's by its id there, 12@vps
  whiff wait <id>                        print the answer to a pending message or job, waiting for it
  whiff cancel <id>                      cancel a task that hasn't ended: held, waiting for a reply or a running job
  whiff pending                          list agents' messages that wait for your review, here and on each peer, as 12@vps
  whiff accept <id>                      deliver a message that waits for review, and print its value; 12@vps on a peer
  whiff reject <id> <reason>             answer a message that waits for review with (err {:kind :rejected})
  whiff trust <agent/n> <service>...     let an agent session's messages to the services, or functions
                                         as board/build, through, as if you accepted them
  whiff free <agent/n>                   let everything an agent session sends, publishes and evaluates through
  whiff gate <agent/n>                   hold an agent session's :write messages for review again, as by default
  whiff session <agent/n>                what an agent session wrote, and how: held, accepted, trusted or free
  whiff eval <code>                      evaluate puff code in the daemon and print its value
  whiff repl                             evaluate forms as they're typed, in the daemon, keeping what they define
  whiff history [<query>]                print the last 1000 messages, newest first; with query, what it matches of 30 days'
  whiff history -f [<query>]             print the last 10, oldest first, then each new one until stopped
  whiff history -c [<query>]             print it as chains: each message that began one, newest first, then what it led to, indented
                                         query: to:plan from:agent origin:user kind:err 'time:>500ms' mark:write
                                         gate:held project:tally peer:vps, a word, -term for what must not match
  whiff services                         list the services, what needs you first: failed, faulting, waiting for you, busy, then the rest
  whiff services <service>               one service in full: its file, last message, faults, inbox and timers; an agent's turns and spend
  whiff ls                               list every address's functions and their marks, each agent session's mode, then the :read overrides
  whiff state <service>                  print a service's state, as it last committed it, as call does
  whiff dump [<bucket>]                  list whiff.db's buckets, or print one's entries: key value,
                                         a line each when piped, for grep; broken over lines on a terminal
  whiff reset <service>                  forget a service's stored state, so it starts from its :state again
  whiff mcp [--dir <path>]               serve whiff to an agent over MCP on stdin and stdout, as :agent/n, its calls from path
  whiff sandbox check                    say which container runtime confines sandboxed commands, and whether its image is built
  whiff sandbox build                    build the image sandboxed commands run in, with that runtime
  whiff peer add <name> <user@host> [--dir <path>] [--whiff <path>]
                                         reach another machine's whiff, over ssh, as you: its calls run in
                                         dir there, and whiff is its path there if it is neither on ssh's
                                         PATH nor in ~/go/bin
  whiff peer ls                          list the peers: connected, connecting or offline, and why
  whiff peer rm <name>                   disconnect a peer and forget it
  whiff bridge                           connect stdin and stdout to the daemon, starting it if need be:
                                         how another machine's whiff reaches this one, over ssh
  whiff stop                             stop the daemon
  whiff daemon                           run the daemon in the foreground

trust and free take --for <d>, as --for 2h or 3d, to end the mode after that
long, and --most <n> to end it after n writes it let through; else it lasts
until the session ends. Either way, its end tells you, in your inbox. An agent
on a peer is agent/lead@vps: the peer keeps to its mode.
  whiff trust agent/lead board/plan board/build --most 20 --for 3d

Each arg of call, send, pub, try and reply is read as a puff value, or taken as
a string if it doesn't read as one: 12 is 12, ann is "ann", '"12"' is "12".
A reply that isn't (ok v) or (err e) is sent as (ok value).

A view prints as lines: items as label, detail and action apart by tabs,
locations as file:line:col: note, text as it is, a board as a "# title"
line per column above its items. Pick items with fzf and
act on them: whiff call todo/view | fzf | whiff act
`

// stdin is what whiff repl reads; a var so tests can type into it.
var stdin io.Reader = os.Stdin

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "whiff: %v\n", err)
		os.Exit(1)
	}
	services := filepath.Join(home, ".whiff", "services")
	os.Exit(run(os.Args[1:], rpc.DefaultDir(), services, os.Stdout, os.Stderr))
}

// run is whiff with its arguments, the socket and services directories and
// its streams, returning the exit code.
func run(args []string, dir, services string, stdout, stderr io.Writer) int {
	switch {
	case len(args) >= 4 && (args[0] == "call" || args[0] == "send") && args[1] == "-C":
		return messageThere(dir, args[0], args[3], args[2], args[4:], stdout, stderr)
	case len(args) >= 2 && (args[0] == "call" || args[0] == "send"):
		return message(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) >= 1 && args[0] == "sub":
		return sub(dir, args[1:], stdout, stderr)
	case len(args) >= 3 && args[0] == "try":
		return try(dir, args[1], args[2], args[3:], stdout, stderr)
	case len(args) >= 2 && args[0] == "test":
		return test(dir, args[1:], stdout, stderr)
	case len(args) == 3 && args[0] == "pub":
		return message(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) == 3 && args[0] == "reply", len(args) == 2 && args[0] == "wait",
		len(args) == 2 && args[0] == "accept", len(args) == 3 && args[0] == "reject":
		return message(dir, args[0], "", args[1:], stdout, stderr)
	case len(args) == 2 && args[0] == "cancel":
		return message(dir, "cancel-task", "", args[1:], stdout, stderr)
	case len(args) >= 2 && (args[0] == "trust" && len(args) >= 3 || args[0] == "free" || args[0] == "gate"):
		return setMode(dir, args[0], args[1], args[2:], stdout, stderr)
	case len(args) == 2 && args[0] == "session":
		session := func(c *rpc.Client) (string, error) {
			es, err := c.History()
			return rpc.SessionText(es, args[1]), err
		}
		return text(dir, session, stdout, stderr)
	case len(args) >= 1 && args[0] == "act":
		return act(dir, args[1:], stdin, stdout, stderr)
	case len(args) == 1 && args[0] == "pending":
		pending := func(c *rpc.Client) (string, error) { return inboxes(c, "review") }
		return text(dir, pending, stdout, stderr)
	case len(args) <= 2 && len(args) >= 1 && args[0] == "inbox":
		addr := "me"
		if len(args) == 2 {
			addr = args[1]
		}
		inbox := func(c *rpc.Client) (string, error) { return inboxes(c, addr) }
		return text(dir, inbox, stdout, stderr)
	case len(args) == 1 && args[0] == "repl":
		return repl(dir, stdin, stdout, stderr)
	case len(args) == 2 && args[0] == "eval":
		return eval(dir, args[1], stdout, stderr)
	case len(args) >= 1 && args[0] == "history":
		flag := ""
		if len(args) > 1 && (args[1] == "-f" || args[1] == "-c") {
			flag, args = args[1], args[1:]
		}
		return history(dir, flag, strings.Join(args[1:], " "), stdout, stderr)
	case len(args) == 1 && args[0] == "services":
		return listServices(dir, stdout, stderr)
	case len(args) == 2 && args[0] == "services":
		return showService(dir, args[1], stdout, stderr)
	case len(args) == 1 && args[0] == "ls":
		return text(dir, (*rpc.Client).Ls, stdout, stderr)
	case len(args) == 2 && (args[0] == "reset" || args[0] == "state"):
		return serviceOp(dir, args[0], args[1], stdout, stderr)
	case len(args) <= 2 && len(args) >= 1 && args[0] == "dump":
		return text(dir, func(c *rpc.Client) (string, error) { return dump(c, args[1:], prettyWidth(stdout)) }, stdout, stderr)
	case len(args) == 1 && args[0] == "mcp":
		return mcpRun(dir, stderr)
	case len(args) == 3 && args[0] == "mcp" && args[1] == "--dir":
		// An agent's turn: its tools act in the project, not its home.
		if err := os.Chdir(args[2]); err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		return mcpRun(dir, stderr)
	case len(args) == 2 && args[0] == "sandbox" && (args[1] == "check" || args[1] == "build"):
		return sandboxCmd(args[1], stdout, stderr)
	case len(args) >= 2 && args[0] == "peer":
		return peerCmd(dir, args[1:], stdout, stderr)
	case len(args) == 1 && args[0] == "bridge":
		return bridge(dir, stdin, stdout, stderr)
	case len(args) == 1 && args[0] == "stop":
		return stop(dir, stderr)
	case len(args) == 1 && args[0] == "daemon":
		return runDaemon(dir, services, stderr)
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help"):
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// connect connects to the daemon, starting it if it isn't running, and
// checks it speaks this whiff's protocol. On failure it reports why and
// returns nil.
func connect(dir string, stderr io.Writer) *rpc.Client {
	c, err := rpc.Dial(dir, startDaemon)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return nil
	}
	if err := c.Compatible("whiff"); err != nil {
		c.Close()
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return nil
	}
	return c
}

// message calls, sends, publishes, replies, waits, accepts or rejects (op).
// A call, wait or accept prints the value of (ok v), a string as it is; (err
// e) prints e and exits 1. The others print nothing. A reject's reason is
// a string, whatever it reads as.
func message(dir, op, target string, args []string, stdout, stderr io.Writer) int {
	return messageThere(dir, op, target, "", args, stdout, stderr)
}

// messageThere is message for a call or send run in there: a directory on
// the target's peer, for a far address, or one relative to this one.
func messageThere(dir, op, target, there string, args []string, stdout, stderr io.Writer) int {
	vals := make([]puff.Value, len(args))
	for i, a := range args {
		vals[i] = shellArg(a)
	}
	if op == "reject" {
		vals[1] = puff.String(args[1])
	}
	if op == "reply" {
		if r, ok := vals[1].(puff.Tag); !ok || r.Len() != 1 || r.Name() != "ok" && r.Name() != "err" {
			vals[1] = puff.Ok(vals[1])
		}
	}
	// 12@vps is a peer's message, as whiff inbox and pending show it.
	peer := ""
	if target == "" && len(args) > 0 {
		if id, p, ok := strings.Cut(args[0], "@"); ok {
			if op == "wait" {
				fmt.Fprintf(stderr, "whiff: wait for a peer's task by the id it has here, which whiff call printed\n")
				return 2
			}
			n, err := strconv.ParseInt(id, 10, 64)
			if err != nil || p == "" {
				fmt.Fprintf(stderr, "whiff: want a message id, as 12 or 12@vps, got %s\n", args[0])
				return 2
			}
			vals[0], peer = puff.Int(n), p
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Forward(rpc.Request{Op: op, Target: target, Args: puff.Print(puff.NewVector(vals...)), Dir: wd, There: there, Peer: peer})
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return result(op, resp, stdout, stderr)
}

// inboxes is addr's inbox as text, a message a line; with every connected
// peer's addr's after it, for :me and :review, each id with the peer's
// name, 12@vps, as reply, accept, reject and cancel take it, and a line
// for each peer that isn't connected. addr@peer is only that peer's.
func inboxes(c *rpc.Client, addr string) (string, error) {
	addr = strings.TrimPrefix(addr, ":")
	far := func(addr, peer string) (string, error) {
		resp, err := c.Forward(rpc.Request{Op: "inbox", Target: addr, Peer: peer})
		if err == nil && resp.Error != "" {
			err = errors.New(resp.Error)
		}
		var b strings.Builder
		for line := range strings.Lines(resp.Out) {
			id, rest, _ := strings.Cut(line, "  ")
			b.WriteString(id + "@" + peer + "  " + rest)
		}
		return b.String(), err
	}
	if at := strings.LastIndexByte(addr, '@'); at > 0 {
		return far(addr[:at], addr[at+1:])
	}
	text, err := c.Inbox(addr)
	if err != nil || addr != "me" && addr != "review" {
		return text, err
	}
	resp, err := c.Message("call", "peers/list", "[]", "")
	if err != nil {
		return text, err
	}
	v, _ := puff.ReadValue(resp.Value)
	peers, _ := puff.OkValue(v).(puff.Vector)
	for i := range peers.Len() {
		m, _ := peers.At(i).(puff.Map)
		name, _ := puff.Field(m, "name").(puff.String)
		status, _ := puff.Field(m, "status").(puff.Keyword)
		if status != "connected" && status != "connecting" {
			text += fmt.Sprintf("peer %s is %s: its inbox isn't shown\n", name, status)
			continue
		}
		t, err := far(addr, string(name))
		if err != nil {
			t = fmt.Sprintf("peer %s: %v\n", name, err)
		}
		text += t
	}
	return text, nil
}

// try loads file under a scratch address, calls its function fn with args
// and prints the reply as call does. A file that doesn't load prints its
// error, at file:line:col, and exits 1.
func try(dir, file, fn string, args []string, stdout, stderr io.Writer) int {
	src, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	vals := make([]puff.Value, len(args))
	for i, a := range args {
		vals[i] = shellArg(a)
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Try(file, string(src), fn, puff.Print(puff.NewVector(vals...)), wd)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	}
	return result("call", resp, stdout, stderr)
}

// test runs the tests in each service file, from here, printing each that
// fails and how many passed: 0 if they all did, 1 if any failed or a file
// didn't load. A file that isn't formatted, as puff -f would, fails too.
func test(dir string, files []string, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	passed, failed := 0, 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			failed++
			continue
		}
		// A file that doesn't read is left for the daemon to say why.
		if out, err := puff.Format(file, string(src)); err == nil && out != string(src) {
			fmt.Fprintf(stdout, "FAIL %s: not formatted; puff -f %s\n", file, file)
			failed++
		}
		resp, err := c.Test(file, string(src), wd)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		if resp.Error != "" {
			fmt.Fprintln(stderr, resp.Error)
			failed++
			continue
		}
		v, _ := puff.ReadValue(resp.Value)
		results, ok := v.(puff.Vector)
		if !ok {
			fmt.Fprintf(stderr, "whiff: want test results, got %s\n", resp.Value)
			return 1
		}
		for i := range results.Len() {
			r, _ := results.At(i).(puff.Map)
			field := func(k string) string {
				v := puff.Field(r, k)
				s, _ := v.(puff.String)
				return string(s)
			}
			if msg := field("message"); msg != "" {
				fmt.Fprintf(stdout, "FAIL %s %s: %s\n", field("at"), field("test"), msg)
				failed++
			} else {
				passed++
			}
		}
	}
	fmt.Fprintf(stdout, "%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// result prints resp's Result for op: the value of (ok v) for a call, a
// wait, an accept or a state, a view as lines, a string as it is, any
// other value on a line, or, on a terminal, broken over lines to fit; (err
// e) prints e and exits 1, except a pending call, which prints how to
// wait for its answer.
func result(op string, resp rpc.Response, stdout, stderr io.Writer) int {
	v, err := puff.ReadValue(resp.Value)
	r, ok := v.(puff.Tag)
	if err != nil || !ok || r.Len() != 1 {
		fmt.Fprintf(stderr, "whiff: want a Result, got %s\n", resp.Value)
		return 1
	}
	e, _ := r.At(0).(puff.Map)
	kind := puff.Field(e, "kind")
	id := puff.Field(e, "id")
	switch {
	case r.Name() == "err" && op == "call" && puff.Equal(kind, puff.Keyword("pending")) && id != nil:
		fmt.Fprintf(stdout, "pending %s (whiff wait %[1]s)\n", puff.Print(id))
	case r.Name() == "err":
		fmt.Fprintf(stderr, "whiff: %s", errText(r.At(0)))
		return 1
	case op == "call" || op == "wait" || op == "accept" || op == "state":
		if text, ok := viewText(r.At(0)); ok {
			io.WriteString(stdout, text)
		} else if s, ok := r.At(0).(puff.String); ok {
			fmt.Fprintln(stdout, string(s))
		} else if w := prettyWidth(stdout); w > 0 {
			fmt.Fprintln(stdout, puff.PrintPretty(r.At(0), 0, w))
		} else {
			fmt.Fprintln(stdout, puff.Print(r.At(0)))
		}
	}
	return 0
}

// setMode sets agent's mode: trust (with services), free or gate, which
// takes no args. --for 2h, before or among the services, ends it after
// that long, and --most 20 after that many writes. agent@peer is a peer's.
func setMode(dir, op, agent string, args []string, stdout, stderr io.Writer) int {
	var services []string
	var d time.Duration
	most := 0
	for i := 0; i < len(args); i++ {
		if args[i] != "--for" && args[i] != "--most" {
			services = append(services, args[i])
			continue
		}
		ok := i+1 < len(args) && op != "gate"
		if ok && args[i] == "--for" {
			v, err := puff.ReadValue(args[i+1])
			dur, isDur := v.(puff.Duration)
			d, ok = time.Duration(dur), err == nil && isDur && dur > 0
		} else if ok {
			n, err := strconv.Atoi(args[i+1])
			most, ok = n, err == nil && n > 0
		}
		if !ok {
			fmt.Fprint(stderr, usage)
			return 2
		}
		i++
	}
	if op == "gate" && len(services) > 0 || op == "free" && len(services) > 0 || op == "trust" && len(services) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	mode := map[string]string{"trust": "trusted", "free": "free", "gate": "gated"}[op]
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	addr, peer := strings.TrimPrefix(agent, ":"), ""
	if at := strings.LastIndexByte(addr, '@'); at > 0 {
		addr, peer = addr[:at], addr[at+1:]
	}
	req := rpc.ModeRequest(addr, mode, services, d, most)
	req.Peer = peer
	resp, err := c.Forward(req)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	code := result(op, resp, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stdout, modeLine(agent, mode, services, d, most))
	}
	return code
}

// modeLine says what setting a mode did: agent/3 is trusted :gh :todo,
// for 2h0m0s.
func modeLine(agent, mode string, services []string, d time.Duration, most int) string {
	s := strings.TrimPrefix(agent, ":") + " is " + mode
	for _, name := range services {
		s += " :" + strings.TrimPrefix(name, ":")
	}
	if most > 0 {
		s += fmt.Sprintf(", for %d writes", most)
	}
	if d > 0 {
		s += ", for " + puff.Print(puff.Duration(d))
	}
	return s
}

// serviceOp evaluates (op :service), reset or state: reset has the daemon
// forget service's stored state and reload it, and prints nothing; state
// prints its state. (err e) prints e and exits 1.
func serviceOp(dir, op, service string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval("(service/"+op+" "+puff.Print(puff.Keyword(strings.TrimPrefix(service, ":")))+")", "")
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	}
	return result(op, resp, stdout, stderr)
}

// sub prints each value published to topics, every topic if there are
// none, a line each, until the daemon stops:
//
//	09:00:40.788  :jobs/done  user   :jobs  {:id 3 ...}
func sub(dir string, topics []string, stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	if _, err := c.Sub(topics...); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	for e := range c.Events() {
		from := "client"
		if e.From != "" {
			from = ":" + e.From
		}
		fmt.Fprintf(stdout, "%s  :%s  %-5s  %s  %s\n", e.At.Local().Format("15:04:05.000"), e.Topic, e.Origin, from, e.Value)
	}
	return 0
}

// history prints history newest first; with flag -c, as chains; with -f,
// to follow it, the last 10 entries oldest first and then each new one,
// until the daemon stops.
func history(dir, flag, query string, stdout, stderr io.Writer) int {
	q, err := rpc.ParseQuery(query)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 2
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	// A query with mark: needs every function's mark: fetched once, and
	// again on a connection of their own when a call is to one they don't
	// know, at most every two seconds.
	var marks map[string]string
	var fetched time.Time
	marksFor := func(e rpc.Entry, fresh func() (map[string]string, error)) map[string]string {
		if !q.Marks() || (marks != nil && ((e.Kind != "call" && e.Kind != "send") || marks[e.To] != "")) || time.Since(fetched) < 2*time.Second {
			return marks
		}
		if ms, err := fresh(); err == nil {
			marks, fetched = ms, time.Now()
		}
		return marks
	}
	match := func(es []rpc.Entry, fresh func() (map[string]string, error)) []rpc.Entry {
		var got []rpc.Entry
		for _, e := range es {
			if q.Match(e, marksFor(e, fresh)) {
				got = append(got, e)
			}
		}
		return got
	}
	if flag != "-f" {
		// A query looks through all of history; without one, the last
		// 1000 entries are plenty.
		get := c.History
		if query != "" {
			get = c.AllHistory
		}
		es, err := get()
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		text := rpc.HistoryText
		if flag == "-c" {
			text = rpc.ChainsText
		}
		io.WriteString(stdout, text(match(es, c.Marks)))
		return 0
	}
	// Following, c traces history: marks come over a connection of their
	// own.
	fresh := func() (map[string]string, error) {
		mc, err := rpc.Dial(dir, nil)
		if err != nil {
			return nil, err
		}
		defer mc.Close()
		return mc.Marks()
	}
	es, err := c.Trace()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	es = match(es, fresh)
	for _, e := range es[max(len(es)-10, 0):] {
		fmt.Fprintln(stdout, e)
	}
	for e := range c.Entries() {
		if q.Match(e, marksFor(e, fresh)) {
			fmt.Fprintln(stdout, e)
		}
	}
	return 0
}

// shellArg reads a command-line argument as a puff value, or takes it as a
// string if it doesn't read as one.
func shellArg(s string) puff.Value {
	if v, err := puff.ReadValue(s); err == nil {
		return v
	}
	return puff.String(s)
}

// eval prints src's value as the daemon evaluates it: exit 0, 1 for an
// error, 70 for a fault, like puff -e.
func eval(dir, src string, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	resp, err := c.Eval(src, wd)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	io.WriteString(stdout, resp.Out)
	switch {
	case resp.Fault:
		fmt.Fprintln(stderr, resp.Error)
		return 70
	case resp.Error != "":
		fmt.Fprintln(stderr, resp.Error)
		return 1
	case resp.Value != "":
		fmt.Fprintln(stdout, resp.Value)
	}
	return 0
}

// terminal says whether f is a terminal: /dev/null is a character device
// too, but gets no prompt.
func terminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// repl reads puff from in a line at a time and evaluates it in the daemon,
// in one env for the session, printing each value; it keeps reading while a
// form or string is still open. An interrupt cancels the evaluation, or ends
// the REPL at its prompt.
func repl(dir string, in io.Reader, stdout, stderr io.Writer) int {
	f, ok := in.(*os.File)
	interactive := ok && terminal(f)
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()

	var running atomic.Bool
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		for range interrupts {
			if running.Load() {
				c.Cancel()
				continue
			}
			fmt.Fprintln(stdout)
			os.Exit(130)
		}
	}()
	// evalInput reports whether the daemon is still there.
	evalInput := func(src string) bool {
		running.Store(true)
		resp, err := c.Repl(src, wd)
		running.Store(false)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return false
		}
		io.WriteString(stdout, resp.Out)
		switch {
		case resp.Error != "":
			fmt.Fprintln(stderr, resp.Error)
		case resp.Value != "":
			fmt.Fprintln(stdout, resp.Value)
		}
		return true
	}

	sc := bufio.NewScanner(in)
	var buf strings.Builder
	for {
		if interactive {
			if buf.Len() == 0 {
				fmt.Fprint(stdout, "whiff> ")
			} else {
				fmt.Fprint(stdout, "   ... ")
			}
		}
		if !sc.Scan() {
			break
		}
		buf.WriteString(sc.Text())
		buf.WriteByte('\n')
		var perr *puff.Error
		if _, err := puff.Read("", buf.String()); errors.As(err, &perr) && perr.Unclosed {
			continue
		}
		if !evalInput(buf.String()) {
			return 1
		}
		buf.Reset()
	}
	if interactive {
		fmt.Fprintln(stdout)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	if buf.Len() > 0 && !evalInput(buf.String()) {
		return 1
	}
	return 0
}

// text prints what get asks the daemon for: history or ls.
func text(dir string, get func(*rpc.Client) (string, error), stdout, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	defer c.Close()
	h, err := get(c)
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	io.WriteString(stdout, h)
	return 0
}

// dump is whiff dump's output: with no bucket, each bucket in whiff.db
// and how many entries it holds; with one, its entries, its key and its
// value printed as puff, a line each for grep, or, if width isn't 0,
// pretty, as dumpEntry has it.
func dump(c *rpc.Client, bucket []string, width int) (string, error) {
	expr := "(store/buckets)"
	if len(bucket) == 1 {
		expr = "(store/dump " + puff.Print(puff.String(bucket[0])) + ")"
	}
	resp, err := c.Eval(expr, "")
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	v, err := puff.ReadValue(resp.Value)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(bucket) == 0 {
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for i := range v.(puff.Vector).Len() {
			m := v.(puff.Vector).At(i).(puff.Map)
			name, _ := m.Get(puff.Keyword("name"))
			n, _ := m.Get(puff.Keyword("entries"))
			fmt.Fprintf(w, "%s\t%s\n", name.(puff.String), puff.Print(n))
		}
		w.Flush()
		return b.String(), nil
	}
	r := v.(puff.Tag)
	if r.Name() == "err" {
		e := r.At(0).(puff.Map)
		msg, _ := e.Get(puff.Keyword("message"))
		return "", errors.New(string(msg.(puff.String)))
	}
	entries := r.At(0).(puff.Vector)
	for i := range entries.Len() {
		kv := entries.At(i).(puff.Vector)
		b.WriteString(dumpEntry(kv.At(0), kv.At(1), width) + "\n")
	}
	return b.String(), nil
}

// dumpEntry is an entry of whiff dump: key, then its value, on one line if
// width is 0; else the value pretty within width, after a short key, as an
// id, or on the lines below a long one, as a timer's [due id].
func dumpEntry(key, val puff.Value, width int) string {
	k := puff.Print(key)
	switch {
	case width == 0:
		return k + " " + puff.Print(val)
	case len(k) <= 20:
		return k + " " + puff.PrintPretty(val, len(k)+1, width)
	default:
		return k + "\n  " + puff.PrintPretty(val, 2, width)
	}
}

// prettyWidth is how wide w is, if it is a terminal, for puff values
// printed there to be pretty in; else 0, for a value a line, for grep.
func prettyWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !terminal(f) {
		return 0
	}
	if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 {
		return cols
	}
	return 80
}

// peerCmd is whiff peer add, ls and rm: calls to :peers.
func peerCmd(dir string, args []string, stdout, stderr io.Writer) int {
	switch {
	case args[0] == "ls" && len(args) == 1:
		return message(dir, "call", "peers/view", nil, stdout, stderr)
	case args[0] == "rm" && len(args) == 2:
		return message(dir, "call", "peers/rm", []string{puff.Print(puff.String(args[1]))}, stdout, stderr)
	case args[0] == "add" && len(args) >= 3:
		es := []puff.Entry{puff.Key("dest", puff.String(args[2]))}
		for rest := args[3:]; len(rest) > 0; rest = rest[2:] {
			if len(rest) < 2 || rest[0] != "--dir" && rest[0] != "--whiff" {
				fmt.Fprint(stderr, usage)
				return 2
			}
			es = append(es, puff.Key(strings.TrimPrefix(rest[0], "--"), puff.String(rest[1])))
		}
		m, err := puff.NewMap(es...)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 2
		}
		return message(dir, "call", "peers/add", []string{puff.Print(puff.String(args[1])), puff.Print(m)}, stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// bridge connects in and out to the daemon, starting it if need be: what
// another machine's whiff runs over ssh to reach this one, as a client
// whose calls are the user's own. It checks the daemon speaks this whiff's
// protocol, then copies bytes both ways on a connection of their own,
// whose hello comes from the other end, until in ends and the daemon has
// answered what came before, or the daemon goes.
func bridge(dir string, in io.Reader, out, stderr io.Writer) int {
	c := connect(dir, stderr)
	if c == nil {
		return 1
	}
	c.Close()
	nc, err := net.Dial("unix", rpc.SocketPath(dir))
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	defer nc.Close()
	go func() {
		io.Copy(nc, in)
		nc.(*net.UnixConn).CloseWrite()
	}()
	if _, err := io.Copy(out, nc); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}

// stop stops the daemon, whatever protocol it speaks.
func stop(dir string, stderr io.Writer) int {
	c, err := rpc.Dial(dir, nil)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		fmt.Fprintln(stderr, "whiff isn't running")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := c.Stop(); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}

func runDaemon(dir, services string, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := daemon.Run(ctx, dir, services); err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	return 0
}

// startDaemon starts `whiff daemon` in its own session, detached from the
// terminal, in / so it doesn't keep a client's directory busy.
func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
