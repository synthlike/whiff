package daemon

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// agentSrc is the turn loop an (agent name ...) file runs.
//
//go:embed agent.puff
var agentSrc string

// agentService expands f, an (agent name :prompt "..." [:model "..."]
// [:can [:addr/fn ...]] [:dir path] [:every d]) read from file, into the
// (service name ...) whiff runs: agent.puff's forms, after defs of
// prompt, model, can, dir ("" for none) and every. Without :every there's
// no wake.
func agentService(f *puff.Form, file string) (*puff.Form, error) {
	if len(f.Items) < 2 || f.Items[1].Kind != puff.FormSymbol {
		return nil, &puff.Error{Pos: f.Pos, Msg: "agent wants its name"}
	}
	opts := map[puff.Keyword]*puff.Form{}
	wants := map[puff.Keyword]string{
		"prompt": ":prompt wants a string, what the agent is for",
		"model":  ":model wants a string, Claude's model name",
		"every":  ":every wants a duration, as 1d",
		"can":    ":can wants the functions the agent may use, as [:gh/prs :todo/add]",
		"dir":    ":dir wants a string, the directory its tools act in",
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
			return nil, &puff.Error{Pos: o.Pos, Msg: "unknown agent option " + o.String() + "; it takes :prompt, :model, :can, :dir and :every"}
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
	if opts["can"] == nil {
		opts["can"] = &puff.Form{Kind: puff.FormVector, Pos: f.Pos}
	}
	dir := ""
	if d := opts["dir"]; d != nil {
		var err error
		if dir, err = agentDir(string(d.Val.(puff.String)), file); err != nil {
			return nil, &puff.Error{Pos: d.Pos, Msg: ":dir: " + err.Error()}
		}
	}
	opts["dir"] = lit(puff.String(dir))
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
	for _, k := range []puff.Keyword{"prompt", "model", "can", "dir", "every"} {
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

// agentDir is :dir d as an absolute directory that is there: ~/ is your
// home, and a relative one is from the agent's file, a symlink's target,
// as use's paths are.
func agentDir(d, file string) (string, error) {
	switch {
	case strings.HasPrefix(d, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, d[2:])
	case !filepath.IsAbs(d):
		real, err := filepath.EvalSymlinks(file)
		if err != nil {
			real = file
		}
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(real), d))
		if err != nil {
			return "", err
		}
		d = abs
	}
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return "", fmt.Errorf("no directory %s", d)
	}
	return filepath.Clean(d), nil
}

// agentOptOK is whether v is a literal of the kind option k wants.
func agentOptOK(k puff.Keyword, v *puff.Form) bool {
	if k == "can" {
		if v.Kind != puff.FormVector {
			return false
		}
		for _, fn := range v.Items {
			if k, ok := fn.Val.(puff.Keyword); fn.Kind != puff.FormLiteral || !ok || !strings.Contains(string(k), "/") {
				return false
			}
		}
		return true
	}
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
