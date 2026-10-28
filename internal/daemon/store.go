package daemon

import (
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// storeTimeout is how long openStore waits for a daemon that is stopping
// to close whiff.db: until its services finish the messages they handle.
const storeTimeout = 10 * time.Second

// stateBucket holds each stateful service's last committed state, printed,
// by name.
var stateBucket = []byte("state")

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
		_, err := tx.CreateBucketIfNotExists(stateBucket)
		return err
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
