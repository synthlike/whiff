package daemon

import (
	"encoding/binary"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
)

// workKept is how far back an agent's work goes in service/status: its
// day, as its budget is.
const workKept = 24 * time.Hour

// workShown is at most how many of an agent's tasks service/status gives,
// the latest.
const workShown = 20

// agentWork is the tasks the agent name's session started in the last
// workKept, oldest first, at most workShown, for service/status: each
// {:id :to :status :at}, :status :waiting or :held for one that hasn't
// ended, with its :fn, or :ended, with its :result, cut down. So you see
// what a lead handed out and how each came back.
func (d *daemon) agentWork(name string, now time.Time) puff.Value {
	from := "agent/" + name
	type item struct {
		id int64
		v  puff.Value
	}
	var items []item
	open := map[int64]bool{}
	d.do(func(st *state) {
		for _, a := range st.asked {
			if a.from != from {
				continue
			}
			to, fn, status := a.addr, a.fn, "waiting"
			if a.held != nil {
				to, fn, status = a.to, a.held.fn, "held"
			}
			open[a.id] = true
			items = append(items, item{a.id, mapOf("id", puff.Int(a.id), "to", puff.Keyword(to), "fn", puff.Keyword(fn),
				"status", puff.Keyword(status), "at", puff.NewTime(a.at))})
		}
	})
	d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(tasksBucket).ForEach(func(k, v []byte) error {
			// Cheap before reading: most tasks are someone else's.
			if !strings.Contains(string(v), ":from :"+from) {
				return nil
			}
			id := int64(binary.BigEndian.Uint64(k))
			get, err := fields(string(v))
			if err != nil || open[id] {
				return nil
			}
			f, _ := get("from").(puff.Keyword)
			ended, ok := get("ended").(puff.Time)
			if string(f) != from || !ok || now.Sub(ended.Go()) > workKept {
				return nil
			}
			by, _ := get("by").(puff.Keyword)
			items = append(items, item{id, mapOf("id", puff.Int(id), "to", by, "status", puff.Keyword("ended"),
				"at", ended, "result", brief(get("result")))})
			return nil
		})
	})
	slices.SortFunc(items, func(a, b item) int { return int(a.id - b.id) })
	if len(items) > workShown {
		items = items[len(items)-workShown:]
	}
	vs := make([]puff.Value, len(items))
	for i, it := range items {
		vs[i] = it.v
	}
	return puff.NewVector(vs...)
}

// brief is a task's result r cut down to what a line about it needs: an
// (ok m) or (err m) keeps m's short entries, as :status, :run and
// :message's first line; (ok text) keeps text's first line, cut to 120;
// any other is printed, so cut.
func brief(r puff.Value) puff.Value {
	tag, ok := r.(puff.Tag)
	if !ok || tag.Len() != 1 {
		return puff.String(short(puff.Print(r)))
	}
	var m puff.Map
	switch v := tag.At(0).(type) {
	case puff.Map:
		m = v
	case puff.String:
		return puff.NewTag(tag.Name(), puff.String(short(string(v))))
	case puff.Keyword, puff.Int, puff.Float, puff.Bool:
		return r
	default:
		return puff.NewTag(tag.Name(), puff.String(short(puff.Print(v))))
	}
	var es []puff.Entry
	for i := range m.Len() {
		e := m.Entry(i)
		switch v := e.Val.(type) {
		case puff.Keyword, puff.Int, puff.Float, puff.Bool:
			es = append(es, e)
		case puff.String:
			if s := short(string(v)); len(s) <= 120 && (len(v) <= 120 || puff.Equal(e.Key, puff.Keyword("message"))) {
				es = append(es, puff.Entry{Key: e.Key, Val: puff.String(s)})
			}
		}
	}
	b, _ := puff.NewMap(es...)
	return puff.NewTag(tag.Name(), b)
}

// short is s's first line, cut to 120 characters.
func short(s string) string {
	s = firstLine(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}
