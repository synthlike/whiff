package daemon

import (
	"math"
	"slices"
	"time"

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
// {:id :to :status :at :children}, :status :waiting or :held for one that
// hasn't ended, with its :fn, or :ended, with its :result, cut down; and
// :children, what its work began, as :tasks/tree gives them. So you see
// what a lead handed out, the jobs each led to, and how each came back.
func (d *daemon) agentWork(name string, now time.Time) puff.Value {
	from := "agent/" + name
	all := d.tasks(func(task) bool { return true }, math.MaxInt, now)
	children := map[int64][]task{}
	var mine []task
	for _, t := range all {
		if t.parent != 0 {
			children[t.parent] = append(children[t.parent], t)
		}
		if t.from == from {
			mine = append(mine, t)
		}
	}
	slices.SortFunc(mine, func(a, b task) int { return int(a.id - b.id) })
	if len(mine) > workShown {
		mine = mine[len(mine)-workShown:]
	}
	vs := make([]puff.Value, len(mine))
	for i, t := range mine {
		kids := puff.Field(t.node(children), "children")
		es := []puff.Entry{puff.Key("id", puff.Int(t.id)), puff.Key("to", puff.Keyword(t.to)),
			puff.Key("status", puff.Keyword(t.status)), puff.Key("at", puff.NewTime(t.at)), puff.Key("children", kids)}
		if t.status == "ended" {
			es = append(es, puff.Key("result", brief(t.result)))
		} else {
			es = append(es, puff.Key("fn", puff.Keyword(t.fn)))
		}
		vs[i], _ = puff.NewMap(es...)
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
