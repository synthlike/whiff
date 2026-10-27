package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/synthlike/whiff/internal/rpc"
)

// try loads req.Src, the service file req.File, at a scratch address,
// :try/<n>/<name>, calls its function req.Target with req.Args, a printed
// vector, for from, and unloads it. The Response's Value is the call's
// Result; a file that doesn't load is its Error, at file:line:col.
func (d *daemon) try(ctx context.Context, from string, req rpc.Request) rpc.Response {
	name := strings.TrimSuffix(filepath.Base(req.File), ".puff")
	addr := fmt.Sprintf("try/%d/%s", d.tries.Add(1), name)
	s, err := d.loadSource(req.File, req.Src, addr)
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
