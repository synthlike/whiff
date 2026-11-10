package daemon

import (
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// subscribe subscribes l's client to the topics in req.Args, a printed
// vector of keywords, every topic if it is empty, and forwards what is
// published to them until the connection ends. The Response's Value is
// (ok :done).
func (d *daemon) subscribe(l *link, req rpc.Request) (*watcher, rpc.Response) {
	v, err := puff.ReadValue(req.Args)
	vec, ok := v.(puff.Vector)
	if err != nil || !ok {
		return nil, rpc.Response{Error: fmt.Sprintf("want topics as a vector of keywords, got %s", req.Args)}
	}
	w := &watcher{ch: make(chan rpc.Event, mailboxSize), agent: req.Agent}
	for i := range vec.Len() {
		k, ok := vec.At(i).(puff.Keyword)
		if !ok || k == "" {
			return nil, rpc.Response{Error: fmt.Sprintf("want a topic like :git/changed, got %s", puff.Print(vec.At(i)))}
		}
		if w.topics == nil {
			w.topics = map[string]bool{}
		}
		w.topics[string(k)] = true
	}
	d.do(func(st *state) { st.subs[w] = true })
	go func() {
		for {
			select {
			case e := <-w.ch:
				if l.conn.Send(rpc.Response{Event: &e}) != nil {
					return
				}
			case <-l.gone:
				return
			}
		}
	}()
	return w, rpc.Response{Value: puff.Print(puff.Done)}
}

// unsubscribe ends w's subscription.
func (d *daemon) unsubscribe(w *watcher) {
	d.do(func(st *state) { delete(st.subs, w) })
}
