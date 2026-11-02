package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// try loads req.Src, the service file req.File, at a scratch address,
// :try/<n>/<name>, calls its function req.Target with req.Args, a printed
// vector, for from, and unloads it. The Response's Value is the call's
// Result; a file that doesn't load is its Error, at file:line:col. Only
// your connections try files: loading one runs its defs and :start as you.
func (d *daemon) try(ctx context.Context, from string, req rpc.Request) rpc.Response {
	if req.Agent {
		return rpc.Response{Error: "an agent's connection can't try files"}
	}
	name := strings.TrimSuffix(filepath.Base(req.File), ".puff")
	addr := fmt.Sprintf("try/%d/%s", d.tries.Add(1), name)
	s, err := d.loadSource(req.File, req.Dir, req.Src, addr, true)
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	// A trial answers its one message, and nothing published meanwhile.
	s.subs = map[string]subscriber{}
	d.do(func(st *state) { st.addrs[addr] = s })
	d.wg.Go(func() { s.run(d.ctx) })
	defer d.remove(s)
	req.Op, req.Target = "call", ":"+addr+"/"+strings.TrimPrefix(req.Target, ":")
	return d.message(ctx, from, req)
}

// testTimeout bounds each test of whiff test.
const testTimeout = 10 * time.Second

// test loads req.Src, the service file req.File, at a scratch address,
// :try/<n>/<name>, without running its :start or taking messages, and runs
// its (test ...)s in order, as you. The Response's Value is a printed
// vector, a {:test :at :message} for each, with :message "" if it passed;
// a file that doesn't load is its Error, at file:line:col. Only your
// connections run tests: a test runs whatever its file says.
func (d *daemon) test(ctx context.Context, req rpc.Request) rpc.Response {
	if req.Agent {
		return rpc.Response{Error: "an agent's connection can't run tests"}
	}
	name := strings.TrimSuffix(filepath.Base(req.File), ".puff")
	addr := fmt.Sprintf("try/%d/%s", d.tries.Add(1), name)
	s, err := d.loadSource(req.File, req.Dir, req.Src, addr, false)
	if err != nil {
		return rpc.Response{Error: err.Error()}
	}
	s.env.Dir = req.Dir
	s.cur.chain, s.cur.user = []string{s.name}, true
	var results []puff.Value
	for _, t := range s.env.Tests() {
		tctx, cancel := context.WithTimeout(ctx, testTimeout)
		msg := s.env.RunTest(tctx, t)
		cancel()
		m, _ := puff.NewMap(
			puff.Entry{Key: puff.Keyword("test"), Val: puff.String(t.Name)},
			puff.Entry{Key: puff.Keyword("at"), Val: puff.String(t.Pos.String())},
			puff.Entry{Key: puff.Keyword("message"), Val: puff.String(msg)},
		)
		results = append(results, m)
	}
	return rpc.Response{Value: puff.Print(puff.NewVector(results...))}
}
