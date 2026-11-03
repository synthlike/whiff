package daemon

import (
	_ "embed"
	"fmt"

	"github.com/synthlike/whiff/internal/puff"
)

// agentSrc is the turn loop an (agent name ...) file runs.
//
//go:embed agent.puff
var agentSrc string

// agentService expands f, an (agent name :prompt "..." [:model "..."]
// [:every d]), into the (service name ...) whiff runs: agent.puff's forms,
// after defs of prompt, model and every. Without :every there's no wake.
func agentService(f *puff.Form) (*puff.Form, error) {
	if len(f.Items) < 2 || f.Items[1].Kind != puff.FormSymbol {
		return nil, &puff.Error{Pos: f.Pos, Msg: "agent wants its name"}
	}
	opts := map[puff.Keyword]*puff.Form{}
	wants := map[puff.Keyword]string{
		"prompt": ":prompt wants a string, what the agent is for",
		"model":  ":model wants a string, Claude's model name",
		"every":  ":every wants a duration, as 1d",
	}
	rest := f.Items[2:]
	for len(rest) > 0 {
		o := rest[0]
		k, ok := o.Val.(puff.Keyword)
		if o.Kind != puff.FormLiteral || !ok {
			return nil, &puff.Error{Pos: o.Pos, Msg: "agent takes options, as :prompt \"...\", not " + o.String()}
		}
		want, ok := wants[k]
		switch {
		case !ok:
			return nil, &puff.Error{Pos: o.Pos, Msg: "unknown agent option " + o.String() + "; it takes :prompt, :model and :every"}
		case opts[k] != nil:
			return nil, &puff.Error{Pos: o.Pos, Msg: "agent takes " + o.String() + " once"}
		case len(rest) < 2 || !agentOptOK(k, rest[1]):
			return nil, &puff.Error{Pos: o.Pos, Msg: want}
		}
		opts[k], rest = rest[1], rest[2:]
	}
	if opts["prompt"] == nil {
		return nil, &puff.Error{Pos: f.Pos, Msg: "agent wants :prompt, a string saying what it is for"}
	}
	lit := func(v puff.Value) *puff.Form { return &puff.Form{Kind: puff.FormLiteral, Pos: f.Pos, Val: v} }
	if opts["model"] == nil {
		opts["model"] = lit(puff.String(""))
	}
	if opts["every"] == nil {
		opts["every"] = lit(puff.Duration(0))
	}
	sym := func(name string) *puff.Form { return &puff.Form{Kind: puff.FormSymbol, Pos: f.Pos, Name: name} }
	list := func(items ...*puff.Form) *puff.Form { return &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items} }

	loop, err := puff.Read("agent.puff", agentSrc)
	if err != nil {
		return nil, fmt.Errorf("whiff's agent.puff: %v", err)
	}
	every := opts["every"].Val.(puff.Duration) > 0
	items := []*puff.Form{sym("service"), f.Items[1]}
	if every {
		start, err := puff.Read("agent.puff", "(fn () (do (send-after every :wake) (ok :waking)))")
		if err != nil {
			return nil, err
		}
		items = append(items, lit(puff.Keyword("start")), start[0])
	}
	for _, k := range []puff.Keyword{"prompt", "model", "every"} {
		items = append(items, list(sym("def"), sym(string(k)), opts[k]))
	}
	for _, g := range loop {
		if !every && isList(g, "on") && len(g.Items) > 1 && isList(g.Items[1], "wake") {
			continue
		}
		items = append(items, g)
	}
	return &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, nil
}

// agentOptOK is whether v is a literal of the kind option k wants.
func agentOptOK(k puff.Keyword, v *puff.Form) bool {
	if v.Kind != puff.FormLiteral {
		return false
	}
	switch k {
	case "every":
		d, ok := v.Val.(puff.Duration)
		return ok && d > 0
	default:
		_, ok := v.Val.(puff.String)
		return ok
	}
}
