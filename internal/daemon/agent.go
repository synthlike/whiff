package daemon

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// agentSrc is the turn loop an (agent name ...) file runs.
//
//go:embed agent.puff
var agentSrc string

// agentService expands f, an (agent name :prompt "..." [:model "..."]
// [:can [:addr/fn ...]] [:dir path] [:limits {...}] [:every d]) read from
// file, into the (service name ...) whiff runs: agent.puff's forms, after
// defs of prompt, model, can, dir ("" for none), limits (with defaults)
// and every. Without :every there's
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
		"limits": ":limits wants {:turn usd :day usd :time d}, any of them: dollars a turn and a day, and how long a turn may run",
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
			return nil, &puff.Error{Pos: o.Pos, Msg: "unknown agent option " + o.String() + "; it takes :prompt, :model, :can, :dir, :limits and :every"}
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
	opts["limits"] = agentLimits(opts["limits"], lit)
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
	for _, k := range []puff.Keyword{"prompt", "model", "can", "dir", "limits", "every"} {
		items = append(items, list(sym("def"), sym(string(k)), opts[k]))
	}
	for _, g := range loop {
		if !every && isList(g, "ask") && len(g.Items) > 1 && isList(g.Items[1], "wake") {
			continue
		}
		items = append(items, g)
	}
	return &puff.Form{Kind: puff.FormList, Pos: f.Pos, Items: items}, nil
}

// Agents' limits without :limits: dollars a turn, dollars a day, and how
// long a turn may run.
const (
	defaultTurnUSD  = 1.0
	defaultDayUSD   = 5.0
	defaultTurnTime = 10 * time.Minute
)

// agentLimits is :limits f, {:turn usd :day usd :time d}, with the
// defaults for those it leaves out, and dollars as floats.
func agentLimits(f *puff.Form, lit func(puff.Value) *puff.Form) *puff.Form {
	vals := map[string]puff.Value{"turn": puff.Float(defaultTurnUSD), "day": puff.Float(defaultDayUSD), "time": puff.Duration(defaultTurnTime)}
	if f != nil {
		for i := 0; i+1 < len(f.Items); i += 2 {
			k := string(f.Items[i].Val.(puff.Keyword))
			switch v := f.Items[i+1].Val.(type) {
			case puff.Int:
				vals[k] = puff.Float(v)
			default:
				vals[k] = v
			}
		}
	}
	m := &puff.Form{Kind: puff.FormMap}
	for _, k := range []string{"turn", "day", "time"} {
		m.Items = append(m.Items, lit(puff.Keyword(k)), lit(vals[k]))
	}
	return m
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
	if k == "limits" {
		if v.Kind != puff.FormMap || len(v.Items) == 0 {
			return false
		}
		for i := 0; i+1 < len(v.Items); i += 2 {
			key, val := v.Items[i], v.Items[i+1]
			k, _ := key.Val.(puff.Keyword)
			if key.Kind != puff.FormLiteral || val.Kind != puff.FormLiteral {
				return false
			}
			switch x := val.Val.(type) {
			case puff.Int:
				if k != "turn" && k != "day" || x <= 0 {
					return false
				}
			case puff.Float:
				if k != "turn" && k != "day" || x <= 0 {
					return false
				}
			case puff.Duration:
				if k != "time" || x <= 0 {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
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
