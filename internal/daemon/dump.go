package daemon

import (
	"encoding/binary"
	"encoding/hex"
	"slices"
	"unicode"
	"unicode/utf8"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// storeBuckets is each bucket in whiff.db, by name, with how many entries
// it holds: [{:name :entries}].
func (d *daemon) storeBuckets() (puff.Value, error) {
	var items []puff.Value
	err := d.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bbolt.Bucket) error {
			m, err := puff.NewMap(puff.Key("name", puff.String(name)), puff.Key("entries", puff.Int(b.Stats().KeyN)))
			items = append(items, m)
			return err
		})
	})
	return puff.NewVector(items...), err
}

// storeDump is bucket's entries in key order, [key value] each, or
// (err {:kind :no-bucket}). A key is read as storedKey does; a value is
// the puff value it holds, printed, or else its text as a string.
func (d *daemon) storeDump(bucket string) (puff.Value, error) {
	var items []puff.Value
	found := false
	err := d.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		found = true
		return b.ForEach(func(k, v []byte) error {
			val, err := puff.ReadValue(string(v))
			if err != nil {
				val = puff.String(v)
			}
			items = append(items, puff.NewVector(storedKey(k), val))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return puff.Errorf("no-bucket", "whiff.db has no bucket %q", bucket), nil
	}
	return puff.Ok(puff.NewVector(items...)), nil
}

// storedKey is a key of whiff.db as puff: a name, as a service's, is a
// string; an id is an int, as idKey makes them, and a timer's due time
// and number are two; anything else is its bytes in hex, as a string.
func storedKey(k []byte) puff.Value {
	if len(k) > 0 && utf8.Valid(k) && !slices.ContainsFunc([]rune(string(k)), func(r rune) bool { return !unicode.IsPrint(r) }) {
		return puff.String(k)
	}
	if len(k) == 8 {
		return puff.Int(int64(binary.BigEndian.Uint64(k)))
	}
	if len(k) == 16 {
		return puff.NewVector(puff.Int(int64(binary.BigEndian.Uint64(k[:8]))), puff.Int(int64(binary.BigEndian.Uint64(k[8:]))))
	}
	return puff.String(hex.EncodeToString(k))
}
