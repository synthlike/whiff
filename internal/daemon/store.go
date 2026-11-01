package daemon

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// storeTimeout is how long openStore waits for a daemon that is stopping
// to close whiff.db: until its services finish the messages they handle.
const storeTimeout = 10 * time.Second

// stateBucket holds each stateful service's last committed state, printed,
// by name.
var stateBucket = []byte("state")

// metaBucket holds nextIDKey, the last id given out, so an id means one
// message even across a restart: a stale whiff reply can't answer another.
var (
	metaBucket = []byte("meta")
	nextIDKey  = []byte("nextID")
)

// meBucket holds what waits in :me's inbox, printed as (inbox) shows it, by
// id, so it reads back oldest first.
var meBucket = []byte("inbox/me")

// openStore opens whiff.db, next to the services directory (so
// ~/.whiff/whiff.db), with its buckets. Only the daemon opens it.
func openStore(services string) (*bbolt.DB, error) {
	dir := filepath.Dir(services)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(filepath.Join(dir, "whiff.db"), 0o600, &bbolt.Options{Timeout: storeTimeout})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{stateBucket, metaBucket, meBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// storedState is name's committed state, printed, if it has one.
func (d *daemon) storedState(name string) (p string, found bool, err error) {
	err = d.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(stateBucket).Get([]byte(name)); v != nil {
			p, found = string(v), true
		}
		return nil
	})
	return p, found, err
}

// stateOf is (ok state) for the service name, as it last committed it, read
// from whiff.db, not from the service, which may be handling a message.
func (d *daemon) stateOf(name string) (puff.Value, error) {
	var s *service
	d.do(func(st *state) { s = st.addrs[name] })
	switch {
	case s == nil || s.client:
		return puff.Errorf("no-address", "no service :%s", name), nil
	case !s.stateful:
		return puff.Errorf("stateless", ":%s has no state", name), nil
	case !s.keep:
		return puff.Errorf("stateless", ":%s is a trial, whose state isn't kept", name), nil
	}
	p, found, err := d.storedState(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return puff.Errorf("stateless", ":%s has no state stored", name), nil
	}
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, fmt.Errorf(":%s's stored state: %v", name, err)
	}
	return puff.Ok(v), nil
}

// storeState commits p as name's state.
func (d *daemon) storeState(name, p string) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stateBucket).Put([]byte(name), []byte(p))
	})
}

// forgetState deletes name's committed state, if any.
func (d *daemon) forgetState(name string) error {
	return d.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stateBucket).Delete([]byte(name))
	})
}

// idKey is id as a key that sorts by id.
func idKey(id int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(id))
}

// newID is a new id for a message answered later, or a job. One that can't
// be stored still works until the next restart, so it is only noted.
func (st *state) newID() int64 {
	st.nextID++
	err := st.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Put(nextIDKey, strconv.AppendInt(nil, st.nextID, 10))
	})
	if err != nil {
		st.note(fmt.Sprintf("the last id, %d, isn't stored: %v", st.nextID, err))
	}
	return st.nextID
}

// keep stores a, in :me's inbox, as it is now.
func (st *state) keep(a *asked) error {
	return st.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(meBucket).Put(idKey(a.id), []byte(puff.Print(a.value())))
	})
}

// forget deletes a, taken from :me's inbox. One that can't be deleted comes
// back after a restart, so it is only noted.
func (st *state) forget(a *asked) {
	err := st.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(meBucket).Delete(idKey(a.id))
	})
	if err != nil {
		st.note(fmt.Sprintf("message %d stays stored in :me's inbox: %v", a.id, err))
	}
}

// load reads what was stored into a new state: :me's inbox and the last id,
// which is at least the last id in the inbox. A message it can't read stays
// stored, and noted, rather than being lost.
func (st *state) load() {
	err := st.db.View(func(tx *bbolt.Tx) error {
		err := tx.Bucket(meBucket).ForEach(func(k, v []byte) error {
			id := int64(binary.BigEndian.Uint64(k))
			st.nextID = max(st.nextID, id)
			a, err := storedMe(id, string(v))
			if err != nil {
				st.note(fmt.Sprintf("message %d in :me's inbox can't be read, and stays stored: %v", id, err))
				return nil
			}
			st.asked = append(st.asked, a)
			return nil
		})
		if err != nil {
			return err
		}
		v := tx.Bucket(metaBucket).Get(nextIDKey)
		if v == nil {
			return nil
		}
		n, err := strconv.ParseInt(string(v), 10, 64)
		st.nextID = max(st.nextID, n)
		return err
	})
	if err != nil {
		st.note(fmt.Sprintf("whiff.db: %v", err))
	}
}

// storedMe is the message id of :me's inbox from p, as keep printed it. It
// comes back without the cause and session it had: message ids and agent
// numbers begin again with each daemon, so they could name another's. Its
// answer begins a chain, and only a stored :user is the user's, so nothing
// here turns an agent's message into yours.
func storedMe(id int64, p string) (*asked, error) {
	v, err := puff.ReadValue(p)
	if err != nil {
		return nil, err
	}
	m, ok := v.(puff.Map)
	if !ok {
		return nil, fmt.Errorf("want a map, got %s", p)
	}
	get := func(key string) puff.Value {
		v, _ := m.Get(puff.Keyword(key))
		return v
	}
	fn, _ := get("fn").(puff.Keyword)
	args, _ := get("args").(puff.Vector)
	from, _ := get("from").(puff.Keyword)
	dir, _ := get("cwd").(puff.String)
	at, _ := get("at").(puff.Time)
	org, _ := get("origin").(puff.Keyword)
	var opts func(puff.Vector) error
	switch fn {
	case "ask":
		opts = askOpts
	case "tell":
		opts = tellOpts
	default:
		return nil, fmt.Errorf("no :me/%s", fn)
	}
	if args.Len() < 1 || args.Len() > 2 {
		return nil, fmt.Errorf(":me/%s takes 1 to 2 arguments, got %d", fn, args.Len())
	}
	if err := opts(args); err != nil {
		return nil, err
	}
	return &asked{ticket: ticket{id: id, from: string(from), dir: string(dir), user: org == origin(true)},
		addr: "me", fn: string(fn), args: args, at: at.Go()}, nil
}
