package daemon

import (
	"fmt"
	"slices"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// cancel ends task id, if it hasn't ended, for session: "" for you, who may
// cancel any, or an agent session, which may cancel only what it began, as
// :review is yours. One held or waiting leaves its inbox and is answered
// (err {:kind :cancelled :outcome :not-started}); a running job's process
// group is killed, and the job answers (err {:kind :cancelled :outcome
// :stopped}), unless it ended first. A service whose inbox it left hears
// of it, as whiff/cancelled, if it would. The canceller gets :done, or (err
// {:kind :ended :result r}) for a task that had ended, which keeps r.
func (d *daemon) cancel(session string, id int64) puff.Value {
	var r puff.Value
	d.do(func(st *state) {
		mine := func(t ticket) bool { return session == "" || t.session == session }
		refused := puff.Errorf("refused", "task %d isn't one your session began", id)
		if j := st.jobs[id]; j != nil {
			if !mine(j.ticket) {
				r = refused
				return
			}
			j.cancel()
			r = puff.Done
			return
		}
		if i := slices.IndexFunc(st.asked, func(a *asked) bool { return a.id == id }); i >= 0 {
			a := st.asked[i]
			if !mine(a.ticket) {
				r = refused
				return
			}
			st.take(a.addr, id)
			cancelled := failed("cancelled", "not-started", "task %d was cancelled before it started", id)
			if a.addr == "review" {
				d.settleHeld(st, a, cancelled)
			} else {
				d.answer(st, a.ticket, a.addr, cancelled)
				d.notifyCancelled(st, st.addrs[a.addr], a)
			}
			r = puff.Done
			return
		}
		t, _, res, err := storedResult(st.db, id)
		switch {
		case err != nil:
			r = puff.Errorf("store", "%v", err)
		case res == nil:
			r = puff.Errorf("no-id", "no task %d is held, waiting or running", id)
		case !mine(t):
			r = refused
		default:
			e := puff.Errorf("ended", "task %d has ended", id).(puff.Tag).At(0).(puff.Map)
			e, _ = e.With(puff.Keyword("result"), res)
			r = puff.NewTag("err", e)
		}
	})
	return r
}

// clientCancel is cancel for a client; req.Args is the printed vector [id].
// An agent's connection cancels for the session whose address it holds.
func (d *daemon) clientCancel(from string, req rpc.Request) rpc.Response {
	v, err := puff.ReadValue(req.Args)
	vec, ok := v.(puff.Vector)
	var id puff.Int
	if err == nil && ok && vec.Len() == 1 {
		id, ok = vec.At(0).(puff.Int)
	}
	if err != nil || !ok || vec.Len() != 1 {
		return rpc.Response{Error: fmt.Sprintf("want [id], got %s", req.Args)}
	}
	if req.Agent && from == "" {
		return rpc.Response{Error: "an agent cancels only its own session's tasks, and this connection holds no address"}
	}
	return rpc.Response{Value: puff.Print(d.cancel(clientLineage(from, req, false).session, int64(id)))}
}
