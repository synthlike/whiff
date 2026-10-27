package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// loadTimeout bounds evaluating a service file; a var so tests can shorten
// it.
var loadTimeout = 5 * time.Second

// service is a loaded (srv name ...): its env and functions belong to the
// goroutine running it.
type service struct {
	name    string
	env     *puff.Env
	fns     map[string]handler
	mailbox chan message
}

// handler is an (on (fn params...) "doc" body...).
type handler struct {
	pos    puff.Pos
	params []string
	doc    string
}

// loadServices loads every .puff file in dir and starts its service. One
// that fails to load is reported to the log and left out.
func (d *daemon) loadServices(dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.puff"))
	for _, file := range files {
		s, err := d.loadService(file)
		if err != nil {
			fmt.Fprintf(d.log, "whiff: %v\n", err)
			continue
		}
		d.do(func(addrs map[string]*service) { addrs[s.name] = s })
		d.wg.Go(func() { s.run(d.ctx) })
	}
}

// loadService reads and evaluates a service file: one (srv name forms...),
// named like the file. Each on becomes a def, so the service calls its own
// functions by plain name.
func (d *daemon) loadService(file string) (*service, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	forms, err := puff.Read(file, string(src))
	if err != nil {
		return nil, err
	}
	if len(forms) != 1 || !isList(forms[0], "srv") {
		return nil, &puff.Error{Pos: puff.Pos{File: file, Line: 1, Col: 1}, Msg: "a service file holds one (srv name ...)"}
	}
	srv := forms[0]
	name := strings.TrimSuffix(filepath.Base(file), ".puff")
	if len(srv.Items) < 2 || srv.Items[1].Kind != puff.FormSymbol || srv.Items[1].Name != name {
		return nil, &puff.Error{Pos: srv.Pos, Msg: fmt.Sprintf("srv wants its name, %s, as in its file name", name)}
	}
	body := srv.Items[2:]
	if len(body) > 0 && body[0].Kind == puff.FormLiteral {
		if _, ok := body[0].Val.(puff.Keyword); ok {
			return nil, &puff.Error{Pos: body[0].Pos, Msg: "unknown srv option " + body[0].String()}
		}
	}

	s := &service{name: name, fns: map[string]handler{}, mailbox: make(chan message, mailboxSize)}
	var loads []*puff.Form
	for _, f := range body {
		if !isList(f, "on") {
			loads = append(loads, f)
			continue
		}
		fn, def, h, err := handlerDef(f)
		if err != nil {
			return nil, err
		}
		s.fns[fn] = h
		loads = append(loads, def)
	}
	s.env = d.newEnv(d.log)
	ctx, cancel := context.WithTimeout(d.ctx, loadTimeout)
	defer cancel()
	if _, err := s.env.LoadForms(ctx, loads); err != nil {
		return nil, err
	}
	return s, nil
}

// handlerDef turns (on (fn params...) "doc" body...) into
// (def (fn params...) body...).
func handlerDef(f *puff.Form) (string, *puff.Form, handler, error) {
	bad := func(msg string) (string, *puff.Form, handler, error) {
		return "", nil, handler{}, &puff.Error{Pos: f.Pos, Msg: msg}
	}
	if len(f.Items) < 4 {
		return bad(`on wants (fn params...), a doc string and a body`)
	}
	sig, doc := f.Items[1], f.Items[2]
	if sig.Kind != puff.FormList || len(sig.Items) == 0 || sig.Items[0].Kind != puff.FormSymbol {
		return bad("on wants (fn params...)")
	}
	h := handler{pos: f.Pos}
	if s, ok := doc.Val.(puff.String); doc.Kind == puff.FormLiteral && ok {
		h.doc = string(s)
	} else {
		return bad("on wants a doc string after (fn params...)")
	}
	for _, p := range sig.Items[1:] {
		if p.Kind != puff.FormSymbol {
			return bad("on's parameters are names")
		}
		h.params = append(h.params, p.Name)
	}
	items := append([]*puff.Form{{Kind: puff.FormSymbol, Pos: f.Items[0].Pos, Name: "def"}, sig}, f.Items[3:]...)
	return sig.Items[0].Name, &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, h, nil
}

func isList(f *puff.Form, head string) bool {
	return f.Kind == puff.FormList && len(f.Items) > 0 && f.Items[0].Kind == puff.FormSymbol && f.Items[0].Name == head
}

// run handles s's messages one at a time until ctx ends.
func (s *service) run(ctx context.Context) {
	for {
		select {
		case m := <-s.mailbox:
			r := s.handle(m)
			if m.reply != nil {
				m.reply <- r
			}
		case <-ctx.Done():
			return
		}
	}
}

// handle runs the function m names and returns its Result, printed. A fault
// in it replies (err {:kind :fault}); the service keeps running.
func (s *service) handle(m message) string {
	h, ok := s.fns[m.fn]
	if !ok {
		return puff.Print(puff.Errorf("no-function", ":%s has no function %s", s.name, m.fn))
	}
	v, err := puff.ReadValue(m.args)
	if err != nil {
		return puff.Print(puff.Errorf("bad-call", "%v", err))
	}
	vec := v.(puff.Vector)
	if vec.Len() != len(h.params) {
		return puff.Print(puff.Errorf("bad-call", ":%s/%s takes %d arguments, got %d", s.name, m.fn, len(h.params), vec.Len()))
	}
	args := make([]puff.Value, vec.Len())
	for i := range args {
		args[i] = vec.At(i)
	}
	f, _ := s.env.Lookup(m.fn)
	s.env.Dir = m.dir
	r, err := s.env.Call(m.ctx, h.pos, f, args...)
	if err != nil {
		return puff.Print(puff.Errorf("fault", "%v", err))
	}
	if t, ok := r.(puff.Tag); !ok || t.Len() != 1 || t.Name() != "ok" && t.Name() != "err" {
		return puff.Print(puff.Errorf("fault", "%s: :%s/%s returned %s, want (ok v) or (err e)", h.pos, s.name, m.fn, puff.Print(r)))
	}
	reply, err := encode(r)
	if err != nil {
		return puff.Print(puff.Errorf("fault", "%s: :%s/%s: %v", h.pos, s.name, m.fn, err))
	}
	return reply
}
