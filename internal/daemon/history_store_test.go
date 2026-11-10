package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/rpc"
)

// History survives a restart, and its ids go on from where they were, so
// a chain from before can't be mistaken for one after.
func TestHistorySurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(service greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name)))
  (on (relay name) "Greets name through a send." (send :hello name)))`,
	})
	done := start(t, dir)
	c := dial(t, dir)
	if r, err := c.Eval(`(call :greet/relay "bob")`, ""); err != nil || r.Error != "" {
		t.Fatalf("relay: %+v %v", r, err)
	}
	historyHas(t, c, `:greet -> :greet/hello "bob"`)
	stopDaemon(t, c, done)

	start(t, dir)
	c = dial(t, dir)
	if r, err := c.Eval(`(call :greet/hello "ann")`, ""); err != nil || r.Error != "" {
		t.Fatalf("hello: %+v %v", r, err)
	}
	historyHas(t, c, `client -> :greet/hello "ann"`)
	es, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := historyOf(c)
	for _, want := range []string{`client -> :greet/relay "bob"  (ok :done)`, `:greet -> :greet/hello "bob"  (ok "hello, bob")`} {
		if !strings.Contains(h, want) {
			t.Errorf("history after a restart has no %s:\n%s", want, h)
		}
	}
	seen := map[int64]bool{}
	var relay, hello, ann int64
	for _, e := range es {
		if e.ID != 0 && seen[e.ID] {
			t.Errorf("id %d is given twice:\n%s", e.ID, h)
		}
		seen[e.ID] = true
		switch {
		case e.To == "greet/relay":
			relay = e.ID
		case e.Args == `["bob"]`:
			hello = e.Cause
		case e.Args == `["ann"]`:
			ann = e.ID
		}
	}
	if relay == 0 || hello != relay || ann <= relay {
		t.Errorf("relay %d, hello's cause %d, ann %d:\n%s", relay, hello, ann, h)
	}
}

// An entry comes back from whiff.db as it went in.
func TestStoredEntry(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 3, 10, 0, 0, 123456789, time.UTC)
	for _, e := range []entry{
		{at: at, kind: "call", id: 7, cause: 3, user: true, from: "agent/1", to: "todo/add", args: `["x \"y\""]`, reply: "(ok :done)", took: 1234567 * time.Nanosecond, gate: "held 9"},
		{at: at, kind: "send", id: 8, to: "gh/tick", args: "[]"},
		{at: at, note: "box.puff:1:1: no closing ')'"},
	} {
		got, err := storedEntry(puff.Print(e.stored()))
		if err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
		if !got.at.Equal(e.at) {
			t.Errorf("at = %v, want %v", got.at, e.at)
		}
		got.at = e.at
		if got != e {
			t.Errorf("stored as %s, read back as %+v, want %+v", puff.Print(e.stored()), got, e)
		}
	}
	if _, err := storedEntry(`{:kind "call"}`); err == nil {
		t.Error("an entry with no :at reads")
	}
}

// At start, history past historyKept goes, the rest stays, and a whiff.db
// that leaves much of itself free is compacted.
func TestHistoryPrunedAndCompacted(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	services := filepath.Join(dir, "services")
	path := filepath.Join(dir, "whiff.db")
	db, err := openStore(services)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 4096)
	var es []entry
	for i := range 1000 {
		es = append(es, entry{at: time.Now().Add(-historyKept - time.Hour), kind: "call", id: int64(i + 1), reply: big})
	}
	es = append(es, entry{at: time.Now().Add(-historyKept + time.Hour), kind: "call", id: 1001, reply: "(ok :young)"})
	if err := storeEntries(db, es, 1001); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.Stat(path)

	db, err = openStore(services)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after, _ := os.Stat(path)
	if after.Size()*4 > before.Size() {
		t.Errorf("whiff.db is %d bytes, from %d: not compacted", after.Size(), before.Size())
	}
	st := &state{db: db}
	db.View(func(tx *bbolt.Tx) error { return st.loadHistory(tx) })
	if len(st.history) != 1 || st.history[0].reply != "(ok :young)" || st.lastMsg != 1001 {
		t.Errorf("history after pruning = %+v, last id %d", st.history, st.lastMsg)
	}
	if _, err := os.Stat(path + ".compact"); !os.IsNotExist(err) {
		t.Errorf("the compacted copy is left: %v", err)
	}
}

// TestAllHistory asks for all of history where whiff.db holds more than
// the last 1000 entries a client gets otherwise: each comes once, oldest
// first, the stored ones and those since, stored yet or not.
func TestAllHistory(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	services := filepath.Join(dir, "services")
	db, err := openStore(services)
	if err != nil {
		t.Fatal(err)
	}
	var es []entry
	for i := range historySize + 200 {
		es = append(es, entry{at: time.Now().Add(-time.Hour), kind: "call", id: int64(i + 1), to: "old/f", reply: "(ok 1)"})
	}
	if err := storeEntries(db, es, int64(len(es))); err != nil {
		t.Fatal(err)
	}
	db.Close()

	writeServices(t, dir, map[string]string{"greet": `(service greet
  (on (hello name) "Greets name." (ok (string/concat "hello, " name))))`})
	start(t, dir)
	c := dial(t, dir)
	for _, who := range []string{"a", "b", "c"} {
		if _, err := c.Message("call", "greet/hello", `["`+who+`"]`, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Some of what is new may not be stored yet; all of it comes, once.
	recent, err := c.History()
	if err != nil || len(recent) != historySize {
		t.Fatalf("History = %d entries, %v; want the last %d", len(recent), err, historySize)
	}
	all, err := c.AllHistory()
	if err != nil {
		t.Fatal(err)
	}
	var old, hellos int
	for i, e := range all {
		switch {
		case e.To == "old/f":
			if e.ID != int64(old+1) || i != old {
				t.Fatalf("entry %d is old/f's %d, want its %d, first", i, e.ID, old+1)
			}
			old++
		case e.To == "greet/hello":
			hellos++
		}
	}
	if old != len(es) || hellos != 3 {
		t.Errorf("AllHistory has %d stored entries and %d hellos; want %d and 3", old, hellos, len(es))
	}
	if !slices.EqualFunc(all[len(all)-len(recent):], recent, func(a, b rpc.Entry) bool { return a.At.Equal(b.At) && a.ID == b.ID && a.Note == b.Note }) {
		t.Error("AllHistory doesn't end as History does")
	}
	// Once all of it is stored, the same again.
	time.Sleep(3 * historyDelay)
	again, _ := c.AllHistory()
	if len(again) != len(all) {
		t.Errorf("AllHistory, all stored, has %d entries; before, %d", len(again), len(all))
	}
}
