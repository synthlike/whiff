package daemon

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// builtins are whiff's functions on top of puff's base table, acting for
// cur. What a service does with messages is bare, as puff's core is: call,
// send, publish, reply...; what inspects or manages whiff is in a module:
// address/, service/, session/. call, send, send-after and publish are
// :write: in a handler, one with
// a literal target or topic counts as its target's mark instead (see
// marker), and under a :read mark they fault only on a :write target.
func (d *daemon) builtins(cur *caller) []*puff.Builtin {
	return append([]*puff.Builtin{
		{Name: "address/describe", Args: "address", Mark: puff.MarkRead,
			Doc: "(ok fns): address's functions, each {:fn :params :doc :mark :form}, and :yours #t on one only your connections may use.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, err := keywordArg(args[0], "an address like :gh")
				if err != nil {
					return nil, err
				}
				return d.marker().describe(addr), nil
			}},
		{Name: "service/reset", Args: "service", Mark: puff.MarkWrite,
			Doc: "Deletes service's stored state and reloads it from its file, so it starts from its :state again: (ok :done), or (err e).",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, err := keywordArg(args[0], "a service like :gh")
				if err != nil {
					return nil, err
				}
				if addr == cur.addr {
					// Its reload would wait for this handler to end.
					return puff.Errorf("bad-call", "a service can't reset itself"), nil
				}
				r := reset{name: addr, done: make(chan puff.Value, 1)}
				select {
				case d.resets <- r:
				case <-c.Ctx.Done():
					return nil, c.Ctx.Err()
				}
				return <-r.done, nil
			}},
		{Name: "address/list", Mark: puff.MarkRead,
			Doc: "(ok addrs): every address, sorted, each {:address :client :state}: :client for a client's, :state for a service with :state.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var addrs []puff.Value
				d.do(func(st *state) {
					for _, name := range slices.Sorted(maps.Keys(st.addrs)) {
						s := st.addrs[name]
						m, _ := puff.NewMap(
							puff.Key("address", puff.Keyword(name)),
							puff.Key("client", puff.Bool(s.client)),
							puff.Key("state", puff.Bool(s.stateful)),
						)
						addrs = append(addrs, m)
					}
				})
				return puff.Ok(puff.NewVector(addrs...)), nil
			}},
		{Name: "service/list", Mark: puff.MarkRead,
			Doc: "(ok services): each service file, what needs you first, as {:service :agent :status :note}: :status one of :failed, :faulting, :over-budget, :paused, :waiting, :thinking, :busy, :idle or :ok, and :note what is most worth knowing, or \"\".",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var all []puff.Value
				d.do(func(st *state) {
					for _, s := range st.statuses(time.Now()) {
						all = append(all, s.value())
					}
				})
				return puff.Ok(puff.NewVector(all...)), nil
			}},
		{Name: "service/status", Args: "service", Mark: puff.MarkRead,
			Doc: "(ok s): service's line of service/list, {:service :agent :status :note}, and what else whiff knows of it: :file, :load-error, :last {:fn :at} handled, :faults today and the last :fault {:at :message}, :inbox and :timers [{:fn :due}]; for an agent :spent, :turns and :day of the last 24 hours, :last-turn, :every, its session's :mode, as (session/mode) gives it, if it isn't gated, and its :work, the tasks its turns started in the last day, at most 20: [{:id :to :status :at :children}], :waiting or :held with their :fn, or :ended with their :result cut short, and :children, what each one's work began, as :tasks/tree gives them, and :actions, as :tasks/view has them. (err {:kind :no-address}) if there is none.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, err := keywordArg(args[0], "a service like :gh")
				if err != nil {
					return nil, err
				}
				return d.serviceStatus(addr), nil
			}},
		{Name: "service/state", Args: "service", Mark: puff.MarkRead,
			Doc: "(ok state): service's state as it last committed it; (err {:kind :stateless}) for a service without :state, (err {:kind :no-address}) if there is none.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				addr, err := keywordArg(args[0], "a service like :gh")
				if err != nil {
					return nil, err
				}
				return d.stateOf(addr)
			}},
		{Name: "inbox", Args: "[fn]", Mark: puff.MarkRead,
			Doc: "This service's messages waiting for a reply, oldest first, each {:id :fn :args :from :cwd :at}; only fn's, a keyword, if given.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service has an inbox")
				}
				fn := ""
				if len(args) > 0 {
					var err error
					if fn, err = keywordArg(args[0], "a function name like :suggest"); err != nil {
						return nil, err
					}
				}
				var items []puff.Value
				d.do(func(st *state) {
					for _, a := range st.inbox(cur.addr) {
						if fn == "" || a.fn == fn {
							items = append(items, a.value())
						}
					}
				})
				return puff.NewVector(items...), nil
			}},
		// reply is :read: an answer from an agent's chain is :agent's and
		// not reviewed, so what its sender does with it is gated there.
		{Name: "reply", Args: "id result", Mark: puff.MarkRead,
			Doc: "Answer the message id in this service's inbox with result: its sender gets [:whiff/done id result]. (err {:kind :no-id}) if it isn't there.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service has an inbox to reply from: a client answers with whiff reply")
				}
				return d.reply(cur.addr, cur.user, args[0], args[1])
			}},
		{Name: "self", Mark: puff.MarkRead,
			Doc: "This service's address, as a keyword: :assistant, or :try/3/assistant in whiff try.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				if cur.addr == "" {
					return nil, errors.New("only a service, or a client that holds an address, has one")
				}
				return puff.Keyword(cur.addr), nil
			}},
		{Name: "session/tasks", Mark: puff.MarkRead,
			Doc: "(ok ids): for an agent, the tasks its turns' session started that haven't ended, oldest first: its messages waiting in an inbox or for your review. (err {:kind :no-session}) for a service that isn't an agent.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var ids []int64
				var r puff.Value
				d.do(func(st *state) { ids, r = st.sessionTasks(cur.addr) })
				if r != nil {
					return r, nil
				}
				var vs []puff.Value
				for _, id := range ids {
					vs = append(vs, puff.Int(id))
				}
				return puff.Ok(puff.NewVector(vs...)), nil
			}},
		{Name: "session/cancel", Mark: puff.MarkWrite,
			Doc: "(ok ids): for an agent, cancels each task its turns' session started that hasn't ended, as whiff cancel does, and gives their ids, oldest first; a service whose inbox one waited in hears whiff/cancelled. (err {:kind :no-session}) for a service that isn't an agent.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var ids []int64
				var r puff.Value
				d.do(func(st *state) { ids, r = st.sessionTasks(cur.addr) })
				if r != nil {
					return r, nil
				}
				// Cancelled one at a time, outside the route goroutine, which
				// cancel waits on; one that ended meanwhile is left out.
				var done []puff.Value
				for _, id := range ids {
					if puff.Equal(d.cancel("", id), puff.Done) {
						done = append(done, puff.Int(id))
					}
				}
				return puff.Ok(puff.NewVector(done...)), nil
			}},
		{Name: "session/mode", Mark: puff.MarkRead,
			Doc: "(ok m): this agent session's mode, {:mode :gated}, {:mode :free} or {:mode :trusted :services [:todo ...]}, with :until, a time, if it runs out. (err {:kind :no-session}) for anyone else.",
			Fn: func(_ *puff.Call, _ []puff.Value) (puff.Value, error) {
				var r puff.Value
				d.do(func(st *state) { r = st.modeValue(cur.addr) })
				return r, nil
			}},
		{Name: "hand-on", Args: "r", Mark: puff.MarkRead,
			Doc: "r; if r is the pending of a job or ask this service started, its answer goes to the caller of the message being handled instead. (err {:kind :no-id}) for another pending.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if cur.handOn == nil {
					return nil, errors.New("only a service's handler can hand on")
				}
				id, ok := pendingID(args[0])
				if !ok {
					return args[0], nil
				}
				if err := cur.handOn(id); err != nil {
					return puff.Errorf("no-id", "%v", err), nil
				}
				return args[0], nil
			}},
		// submitted is the one place a pending becomes a successful
		// submission. A held one counts: it keeps its id from held to
		// ended, so the same whiff/done answers it, with :rejected if you
		// reject it.
		{Name: "submitted", Args: "r", Mark: puff.MarkRead,
			Doc: "(ok id) if r is a pending, of a job, an ask or a message the review gate holds: it was accepted, and [:whiff/done id result] answers it; else r, so a failure to submit stays an err. hand-on wants the pending itself.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				if id, ok := pendingID(args[0]); ok {
					return puff.Ok(puff.Int(id)), nil
				}
				return args[0], nil
			}},
		{Name: "publish", Args: "topic v", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send v to every service subscribed to topic, a keyword: :done at once; pending if the review gate holds it.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				topic, ok := args[0].(puff.Keyword)
				if !ok || topic == "" {
					return nil, fmt.Errorf("want a topic like :git/changed, got %s", puff.Print(args[0]))
				}
				if what, ok := puff.ReadOnly(c.Ctx); ok {
					if d.marker().topic(string(topic)) == puff.MarkWrite {
						return nil, fmt.Errorf(":%s has :write subscribers, and %s is :read", topic, what)
					}
				}
				msg, err := encode(puff.NewVector(args[1]))
				if err != nil {
					return nil, err
				}
				m := message{cause: cur.id, from: cur.addr, topic: string(topic), args: msg, dir: c.Env.Dir, lineage: cur.lineage}
				return d.publishGated(m), nil
			}},
		{Name: "call", Args: "target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target, :address/fn, a message and wait for its Result; (err {:kind :timeout}) after 5s.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := d.outgoing(c, cur, args)
				if err != nil {
					return nil, err
				}
				return d.request(c.Ctx, cur, addr, m)
			}},
		{Name: "send", Args: "target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target, :address/fn, a message without waiting: :done once it is in the mailbox.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				m, addr, err := d.outgoing(c, cur, args)
				if err != nil {
					return nil, err
				}
				if e := d.deliver(addr, m); e != nil {
					return e, nil
				}
				return puff.Done, nil
			}},
		{Name: "send-after", Args: "d target [arg] ...", Mark: puff.MarkWrite, MarkByArgs: true,
			Doc: "Send target a message once d has passed; :done at once.",
			Fn: func(c *puff.Call, args []puff.Value) (puff.Value, error) {
				after, ok := args[0].(puff.Duration)
				if !ok || after < 0 {
					return nil, fmt.Errorf("want a duration of 0s or more, got %s", puff.Print(args[0]))
				}
				m, addr, err := d.outgoing(c, cur, args[1:])
				if err != nil {
					return nil, err
				}
				// A timer's message begins a chain of its own in history, or
				// a service that schedules itself would be one endless chain;
				// it keeps its origin. A service's timers to itself go with
				// the version that set them, so a reload, whose :start sets
				// its own, doesn't double a loop. It is set only once it is
				// stored.
				m.cause = 0
				t := &timer{due: time.Now().Add(time.Duration(after)), addr: addr, m: m}
				if err := d.keepTimer(t); err != nil {
					return puff.Errorf("store", "the timer for :%s wasn't stored: %v", m.to(addr), err), nil
				}
				var stop, placed <-chan struct{}
				if t.own() {
					stop, placed = cur.stop, cur.placed
				}
				d.arm(t, stop, placed)
				return puff.Done, nil
			}},
	}, viewBuiltins()...)
}

// Functions are the names of whiff's functions, sorted: what puff tells
// you are only in whiff.
func Functions() []string {
	var names []string
	for _, b := range (&daemon{}).builtins(&caller{}) {
		names = append(names, b.Name)
	}
	slices.Sort(names)
	return names
}

// keywordArg is v, a keyword, as a string; an error that wants one, as
// want says, for anything else.
func keywordArg(v puff.Value, want string) (string, error) {
	k, ok := v.(puff.Keyword)
	if !ok {
		return "", fmt.Errorf("want %s, got %s", want, puff.Print(v))
	}
	return string(k), nil
}

// outgoing is the message args, a target and its arguments, ask for from
// a call, send or send-after for cur, with the target's address: an error
// if they don't make one, or under a :read mark if it is :write.
func (d *daemon) outgoing(c *puff.Call, cur *caller, args []puff.Value) (message, string, error) {
	m, addr, err := cur.message(c.Env.Dir, args)
	if err == nil {
		err = d.readOnly(c.Ctx, addr, m)
	}
	return m, addr, err
}

// sessionTasks is the tasks the agent addr's turns' session started that
// haven't ended, oldest first; or, for an address that isn't an agent,
// the (err {:kind :no-session}) for it.
func (st *state) sessionTasks(addr string) ([]int64, puff.Value) {
	if s := st.addrs[addr]; s == nil || !s.agent {
		return nil, puff.Errorf("no-session", ":%s isn't an agent", addr)
	}
	var ids []int64
	for _, a := range st.asked {
		if a.from == "agent/"+addr {
			ids = append(ids, a.id)
		}
	}
	return ids, nil
}
