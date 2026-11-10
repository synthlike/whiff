package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// Moving an agent: `whiff move lead vps` pauses lead here, ships it to the
// peer vps, and resumes it there under the same name, as :lead@vps; `whiff
// move lead@vps here` brings it back. It works because an agent is its
// file and the files in its home: its conversation, a huff transcript; its
// spending; its laters. :agents does it in three steps, each a function the
// mover calls where the agent is or is going, so the laptop drives both
// directions and the VPS never reaches back:
//
//	pack name     pauses name and gives it as a bundle, a map
//	adopt bundle  installs it here, under its name, and starts it
//	drop name     removes the packed agent here, once it is adopted there
//
// An agent moves only when it is idle: nothing in its inbox, no turn
// running, no task it started open (a held write, a pipeline run), since
// those are ids of the whiff it is on, which the other doesn't have. Only
// an agent on huff moves: a Claude agent's conversation is in Claude's
// own files on its machine. Nothing is deleted: drop moves the file to
// ~/.whiff/moved and the home aside, and adopt sets aside a home already
// there, and puts back the file drop moved, a symlink kept as one.

// localFiles are files in an agent's home that hold this whiff's ids, of
// turns, messages and jobs: they don't move. paused moves as :paused.
var localFiles = []string{"turns", "asks", "deferred", "current", "failed", "paused"}

// moveWait is how long adopt and drop wait for the supervisor to load or
// remove the agent.
var moveWait = 10 * time.Second

func (d *daemon) agentsService() *service {
	name := func(v puff.Value) (string, error) {
		s, ok := v.(puff.String)
		if !ok || s == "" || strings.ContainsAny(string(s), "/@: ") {
			return "", fmt.Errorf("want an agent's name, a string like \"lead\", got %s", puff.Print(v))
		}
		return string(s), nil
	}
	return &service{name: "agents", d: d, cur: caller{addr: "agents"},
		fns: map[string]handler{
			"pack": {params: []string{"name"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Pauses agent name and gives it as a bundle for adopt, where it goes: (ok bundle). It must be idle and on huff: (err {:kind :busy}) with what it waits on, or (err {:kind :not-movable}). whiff move does it all.",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					n, err := name(args[0])
					if err != nil {
						return nil, err
					}
					return d.packAgent(n), nil
				}},
			"adopt": {params: []string{"bundle"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Installs the agent in bundle, as pack gives it, under its name, and starts it, paused if it was: (ok :adopted). (err {:kind :exists}) if there's one of its name here, (err {:kind :missing}) if its :can names functions that aren't here.",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					return d.adoptAgent(args[0]), nil
				}},
			"drop": {params: []string{"name"}, mark: puff.MarkWrite, marked: true, yours: true,
				doc: "Removes agent name, packed and adopted elsewhere: its file goes to ~/.whiff/moved and its home aside. (ok :dropped); (err {:kind :busy}) if anything came for it since it was packed.",
				native: func(_ message, args []puff.Value) (puff.Value, error) {
					n, err := name(args[0])
					if err != nil {
						return nil, err
					}
					return d.dropAgent(n), nil
				}},
		},
		subs: map[string]subscriber{}, code: map[string][]*puff.Form{},
		mailbox: make(chan message, mailboxSize), quit: make(chan struct{}), done: make(chan struct{})}
}

// agentHome is agent name's home.
func (d *daemon) agentHome(name string) string {
	return filepath.Join(d.home(), ".whiff", "agents", name)
}

// movable checks name is an agent here that can move, and is idle: nil,
// or the error why not. model and can are its :model and :can.
func (d *daemon) movable(name string) (model string, can puff.Vector, res puff.Value) {
	d.do(func(st *state) {
		s := st.addrs[name]
		if s == nil || !s.agent {
			res = puff.Errorf("no-address", "no agent :%s here", name)
			return
		}
		if v, ok := s.env.Lookup("model"); ok {
			m, _ := v.(puff.String)
			model = string(m)
		}
		if v, ok := s.env.Lookup("can"); ok {
			can, _ = v.(puff.Vector)
		}
		if !strings.Contains(model, "/") {
			res = puff.Errorf("not-movable", ":%s runs on the claude CLI, whose conversation is in Claude's own files on this machine: only an agent on huff, a provider's :model, moves", name)
			return
		}
		var waits []string
		for _, a := range st.inbox(name) {
			waits = append(waits, fmt.Sprintf("message %d in its inbox", a.id))
		}
		ids, _ := st.sessionTasks(name)
		for _, id := range ids {
			waits = append(waits, fmt.Sprintf("its task %d", id))
		}
		if len(waits) > 0 {
			res = puff.Errorf("busy", ":%s isn't idle: %s. Let them end, or cancel them (whiff call %s/cancel), and move it then", name, strings.Join(waits, ", "), name)
		}
	})
	return model, can, res
}

// packAgent pauses agent name and is it as a bundle: {:name :source
// :model :can :files {file text} :paused :mode}, :mode if it isn't gated.
// One that isn't idle is unpaused again, as it was.
func (d *daemon) packAgent(name string) puff.Value {
	home := d.agentHome(name)
	pausedFile := filepath.Join(home, "paused")
	b, _ := os.ReadFile(pausedFile)
	wasPaused := strings.TrimSpace(string(b)) == "paused"
	if _, _, res := d.movable(name); res != nil {
		return res
	}
	// Paused first, then checked again: a message that came meanwhile
	// waits, and is seen.
	if err := os.MkdirAll(home, 0o700); err != nil {
		return puff.Errorf("store", "%v", err)
	}
	if err := os.WriteFile(pausedFile, []byte("paused"), 0o600); err != nil {
		return puff.Errorf("store", "%v", err)
	}
	model, can, res := d.movable(name)
	if res != nil {
		if !wasPaused {
			os.WriteFile(pausedFile, nil, 0o600)
		}
		return res
	}
	source, err := os.ReadFile(filepath.Join(d.services, name+".puff"))
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	var files []puff.Entry
	for _, e := range entries {
		if !e.Type().IsRegular() || slices.Contains(localFiles, e.Name()) {
			continue
		}
		text, err := os.ReadFile(filepath.Join(home, e.Name()))
		if err != nil {
			return puff.Errorf("store", "%v", err)
		}
		files = append(files, puff.Entry{Key: puff.String(e.Name()), Val: puff.String(text)})
	}
	fm, err := puff.NewMap(files...)
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	es := []puff.Entry{puff.Key("name", puff.String(name)), puff.Key("source", puff.String(source)),
		puff.Key("model", puff.String(model)), puff.Key("can", can), puff.Key("files", fm), puff.Key("paused", puff.Bool(wasPaused))}
	d.do(func(st *state) {
		if m := st.modeOf("agent/" + name); m != nil {
			es = append(es, puff.Key("mode", m.value()))
		}
	})
	v, err := puff.NewMap(es...)
	if err != nil {
		return puff.Errorf("store", "%v", err)
	}
	return puff.Ok(v)
}

// adoptAgent installs the agent in bundle and starts it.
func (d *daemon) adoptAgent(bundle puff.Value) puff.Value {
	str := func(k string) string { s, _ := puff.Field(bundle, k).(puff.String); return string(s) }
	name, source := str("name"), str("source")
	files, isMap := puff.Field(bundle, "files").(puff.Map)
	can, _ := puff.Field(bundle, "can").(puff.Vector)
	paused, _ := puff.Field(bundle, "paused").(puff.Bool)
	if name == "" || strings.ContainsAny(name, "/@: ") || source == "" || !isMap {
		return puff.Errorf("bad-call", "want a bundle as agents/pack gives it, got %s", puff.Print(bundle))
	}
	var m *mode
	if mv := puff.Field(bundle, "mode"); mv != nil {
		var err error
		if m, err = storedMode(puff.Print(mv)); err != nil {
			return puff.Errorf("bad-call", "its mode: %v", err)
		}
	}
	file := filepath.Join(d.services, name+".puff")
	var res puff.Value
	d.do(func(st *state) {
		if st.addrs[name] != nil || own(name) {
			res = puff.Errorf("exists", "there is an address :%s here already", name)
			return
		}
		var missing []string
		for i := range can.Len() {
			k, _ := can.At(i).(puff.Keyword)
			addr, fn, _ := strings.Cut(string(k), "/")
			if s := st.addrs[addr]; s == nil {
				missing = append(missing, ":"+string(k))
			} else if _, ok := s.fns[fn]; !ok {
				missing = append(missing, ":"+string(k))
			}
		}
		if len(missing) > 0 {
			res = puff.Errorf("missing", ":%s may call %s, which aren't here: install those services first", name, strings.Join(missing, ", "))
		}
	})
	if res != nil {
		return res
	}
	if _, err := os.Lstat(file); err == nil {
		return puff.Errorf("exists", "%s is here already", file)
	}
	// A home already here, from an earlier stay, is set aside.
	home := d.agentHome(name)
	stamp := time.Now().Format("20060102T150405")
	aside := ""
	if _, err := os.Stat(home); err == nil {
		aside = home + ".before-" + stamp
		if err := os.Rename(home, aside); err != nil {
			return puff.Errorf("store", "%v", err)
		}
	}
	undo := func() {
		os.Remove(file)
		os.RemoveAll(home)
		if aside != "" {
			os.Rename(aside, home)
		}
	}
	fail := func(err error) puff.Value { undo(); return puff.Errorf("store", "%v", err) }
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fail(err)
	}
	for i := range files.Len() {
		e := files.Entry(i)
		k, _ := e.Key.(puff.String)
		text, _ := e.Val.(puff.String)
		if k == "" || strings.ContainsAny(string(k), "/\\") || k == "." || k == ".." || slices.Contains(localFiles, string(k)) {
			undo()
			return puff.Errorf("bad-call", "a bundle's file is named %s", puff.Print(e.Key))
		}
		if err := os.WriteFile(filepath.Join(home, string(k)), []byte(text), 0o600); err != nil {
			return fail(err)
		}
	}
	// It starts paused, so nothing runs before it is in place; then as it was.
	if err := os.WriteFile(filepath.Join(home, "paused"), []byte("paused"), 0o600); err != nil {
		return fail(err)
	}
	// The file drop moved here, if it is the same agent, goes back: a
	// symlink into a repository stays one.
	moved := filepath.Join(filepath.Dir(d.services), "moved", name+".puff")
	if b, err := os.ReadFile(moved); err == nil && string(b) == source {
		if err := os.Rename(moved, file); err != nil {
			return fail(err)
		}
	} else if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		return fail(err)
	}
	if err := d.awaitLoad(name, file); err != nil {
		undo()
		return puff.Errorf("load", "%v", err)
	}
	if m != nil {
		d.do(func(st *state) {
			st.setMode("agent/"+name, m)
			st.record(entry{at: time.Now(), note: fmt.Sprintf(":agent/%s is %s, as it was where it came from", name, m)})
		})
	}
	if !paused {
		os.WriteFile(filepath.Join(home, "paused"), nil, 0o600)
	}
	d.do(func(st *state) { st.record(entry{at: time.Now(), note: fmt.Sprintf(":%s moved here", name)}) })
	return puff.Ok(puff.Keyword("adopted"))
}

// awaitLoad waits for the supervisor to load agent name from file: nil,
// or why it didn't.
func (d *daemon) awaitLoad(name, file string) error {
	for deadline := time.Now().Add(moveWait); time.Now().Before(deadline); time.Sleep(pollInterval / 5) {
		var loaded bool
		var why string
		d.do(func(st *state) {
			s := st.addrs[name]
			loaded = s != nil && s.agent
			if h := st.health[name]; h != nil && h.file == file {
				why = h.loadErr
			}
		})
		switch {
		case loaded:
			return nil
		case why != "":
			return fmt.Errorf("it didn't load here: %s", why)
		}
	}
	return fmt.Errorf("it didn't load here within %v", moveWait)
}

// dropAgent removes the packed agent name: its file to ~/.whiff/moved,
// its home aside.
func (d *daemon) dropAgent(name string) puff.Value {
	home := d.agentHome(name)
	if b, _ := os.ReadFile(filepath.Join(home, "paused")); strings.TrimSpace(string(b)) != "paused" {
		return puff.Errorf("not-packed", ":%s isn't paused: pack it first", name)
	}
	if _, _, res := d.movable(name); res != nil {
		return res
	}
	file := filepath.Join(d.services, name+".puff")
	movedDir := filepath.Join(filepath.Dir(d.services), "moved")
	if err := os.MkdirAll(movedDir, 0o700); err != nil {
		return puff.Errorf("store", "%v", err)
	}
	if err := os.Rename(file, filepath.Join(movedDir, name+".puff")); err != nil {
		return puff.Errorf("store", "%v", err)
	}
	for deadline := time.Now().Add(moveWait); ; time.Sleep(pollInterval / 5) {
		var gone bool
		d.do(func(st *state) { gone = st.addrs[name] == nil })
		if gone {
			break
		}
		if time.Now().After(deadline) {
			return puff.Errorf("store", ":%s's file is gone, but it is still here after %v", name, moveWait)
		}
	}
	if err := os.Rename(home, home+".moved-"+time.Now().Format("20060102T150405")); err != nil && !os.IsNotExist(err) {
		return puff.Errorf("store", "%v", err)
	}
	d.do(func(st *state) { st.record(entry{at: time.Now(), note: fmt.Sprintf(":%s moved away", name)}) })
	return puff.Ok(puff.Keyword("dropped"))
}
