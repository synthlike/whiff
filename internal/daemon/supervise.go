package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// pollInterval is how often the supervisor looks at the services directory;
// a var so tests can shorten it.
var pollInterval = 500 * time.Millisecond

// supervisor keeps the services in dir in step with their files: it loads
// new files, reloads changed ones, removes services whose file is gone, and
// reloads a service that faults. It is the only one to start or stop
// services, from one goroutine.
type supervisor struct {
	d     *daemon
	dir   string
	files map[string]*watched // by path
}

// watched is a service file as the supervisor last saw it, and the service
// running from it, nil if it never loaded.
type watched struct {
	mod  time.Time
	size int64
	s    *service
}

func (d *daemon) newSupervisor(dir string) *supervisor {
	return &supervisor{d: d, dir: dir, files: map[string]*watched{}}
}

// run looks at the directory every pollInterval, and restarts the services
// that ask, until the daemon stops.
func (sv *supervisor) run() {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			sv.scan()
		case s := <-sv.d.restarts:
			for file, w := range sv.files {
				// A service replaced since it faulted is left alone.
				if w.s == s {
					sv.d.note(":%s faulted; restarting it from its file", s.name)
					sv.load(file, w)
				}
			}
		case r := <-sv.d.resets:
			r.done <- sv.reset(r.name)
		case <-sv.d.ctx.Done():
			return
		}
	}
}

// scan loads every file that is new or changed since the last scan, and
// removes the services whose file is gone.
func (sv *supervisor) scan() {
	paths, _ := filepath.Glob(filepath.Join(sv.dir, "*.puff"))
	there := map[string]bool{}
	for _, file := range paths {
		fi, err := os.Stat(file)
		if err != nil {
			continue
		}
		there[file] = true
		w := sv.files[file]
		if w == nil {
			w = &watched{}
			sv.files[file] = w
		} else if fi.ModTime().Equal(w.mod) && fi.Size() == w.size {
			continue
		}
		w.mod, w.size = fi.ModTime(), fi.Size()
		sv.load(file, w)
	}
	for file, w := range sv.files {
		if !there[file] {
			if w.s != nil {
				sv.d.remove(w.s)
			}
			delete(sv.files, file)
		}
	}
}

// load loads file and puts it in place of the service running from it, if
// any. A file that fails to load leaves that service running, and the
// error is noted and returned.
func (sv *supervisor) load(file string, w *watched) error {
	s, err := sv.d.loadService(file)
	switch {
	case err != nil && w.s != nil:
		sv.d.note("%v; :%s keeps running as it was", err, w.s.name)
		return err
	case err != nil:
		sv.d.note("%v", err)
		return err
	}
	old := w.s
	if old != nil {
		// The new version takes over the mailbox, and with it the messages
		// waiting there.
		s.mailbox = old.mailbox
	}
	held := false
	sv.d.do(func(st *state) {
		if other := st.addrs[s.name]; other != nil && other.client {
			held = true
			return
		}
		st.addrs[s.name] = s
	})
	if held {
		close(s.quit) // drops the timers its :start set
		err := fmt.Errorf("%s: :%s is held by a client, so the service isn't loaded", file, s.name)
		sv.d.note("%v", err)
		return err
	}
	if old != nil {
		sv.d.stop(old)
	}
	w.s = s
	sv.d.wg.Go(func() { s.run(sv.d.ctx) })
	return nil
}

// reset is whiff reset name, for the supervisor: done gets its Result.
type reset struct {
	name string
	done chan puff.Value
}

// reset deletes service name's stored state and reloads it from its file,
// so it starts from init: (ok :done), or (err e) if there is no such
// service with state, or its file doesn't load, which leaves it running
// with its stored state as it was.
func (sv *supervisor) reset(name string) puff.Value {
	for file, w := range sv.files {
		if w.s == nil || w.s.name != name {
			continue
		}
		if !w.s.stateful {
			return puff.Errorf("stateless", ":%s has no state", name)
		}
		p, found, err := sv.d.storedState(name)
		if err == nil {
			err = sv.d.forgetState(name)
		}
		if err != nil {
			return puff.Errorf("store", "%v", err)
		}
		if err := sv.load(file, w); err != nil {
			if found {
				sv.d.storeState(name, p)
			}
			return puff.Errorf("load", "%v", err)
		}
		return puff.Done
	}
	return puff.Errorf("no-address", "no service :%s", name)
}

// remove takes s's address away and stops it: a service whose file is
// gone, or a client's address when it hangs up. Calls waiting in its
// mailbox get (err {:kind :no-address}), and so do the senders of the
// messages in its inbox, as [:whiff/done id result].
func (d *daemon) remove(s *service) {
	d.do(func(st *state) {
		delete(st.addrs, s.name)
		delete(st.modes, s.name)
		for _, a := range st.inbox(s.name) {
			st.take(s.name, a.id)
			d.answer(st, a.ticket, s.name, puff.Errorf("no-address", "no address :%s", s.name))
		}
	})
	d.stop(s)
	for {
		select {
		case m := <-s.mailbox:
			if m.reply != nil {
				m.reply <- puff.Print(puff.Errorf("no-address", "no address :%s", s.name))
			}
		default:
			return
		}
	}
}

// stop has s finish the message it is handling, if any, and waits for it.
func (d *daemon) stop(s *service) {
	close(s.quit)
	<-s.done
}
