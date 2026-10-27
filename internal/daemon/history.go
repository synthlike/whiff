package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// historySize bounds history, which is in memory until it is stored.
const historySize = 1000

// entry is one line of history: a message and its Result, or a note about
// whiff itself, such as a service that failed to load.
type entry struct {
	at    time.Time
	call  bool   // a call, which waits for its reply; else a send
	from  string // the sender's address; "" for a client
	to    string // address/fn
	args  string // a printed vector
	reply string // the printed Result
	took  time.Duration
	note  string
}

func (st *state) record(e entry) {
	st.history = append(st.history, e)
	if n := len(st.history) - historySize; n > 0 {
		st.history = slices.Delete(st.history, 0, n)
	}
}

// note records something about whiff itself in history, and in the log.
func (d *daemon) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(d.log, "whiff: %s\n", msg)
	d.do(func(st *state) { st.record(entry{at: time.Now(), note: msg}) })
}

// historyText is history newest first, an entry a line.
func (d *daemon) historyText() string {
	var b strings.Builder
	d.do(func(st *state) {
		for _, e := range slices.Backward(st.history) {
			b.WriteString(e.String())
			b.WriteByte('\n')
		}
	})
	return b.String()
}

// String prints e as a line of history:
//
//	10:42:01.123  call  client -> :greet/hello "ann"  (ok "hello, ann")  1.2ms
func (e entry) String() string {
	at := e.at.Format("15:04:05.000")
	if e.note != "" {
		return at + "  note  " + e.note
	}
	kind := "send"
	if e.call {
		kind = "call"
	}
	from := "client"
	if e.from != "" {
		from = ":" + e.from
	}
	target := ":" + e.to
	if args := e.args[1 : len(e.args)-1]; args != "" {
		target += " " + args
	}
	line := fmt.Sprintf("%s  %s  %s -> %s  %s", at, kind, from, target, e.reply)
	if e.took > 0 {
		line += "  " + e.took.Round(time.Microsecond).String()
	}
	return line
}
