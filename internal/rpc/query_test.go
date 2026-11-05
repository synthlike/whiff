package rpc

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQuery(t *testing.T) {
	es := map[string]Entry{
		"draft": {Kind: "call", Origin: "agent", From: "agent/3", To: "plan/draft", Args: `["fix the test"]`, Reply: `(ok 1)`, Took: 2 * time.Second, Gate: "accepted 4", Project: "tally"},
		"board": {Kind: "call", Origin: "user", To: "plan/board", Args: `[]`, Reply: `(ok (items []))`, Took: 3 * time.Millisecond},
		"fault": {Kind: "call", Origin: "agent", From: "plan", To: "gh/pr", Args: `["b"]`, Reply: `(err {:kind :fault :message "gh.puff:14:9"})`, Took: 31 * time.Millisecond, Gate: "for agent/3"},
		"held":  {Kind: "call", Origin: "agent", From: "agent/3", To: "todo/add", Args: `["x"]`, Reply: `(err {:id 2 :kind :pending})`, Gate: "held 2", Project: "tally"},
		"err":   {Kind: "call", Origin: "user", To: "todo/done", Args: `[9]`, Reply: `(err {:kind :no-item})`, Took: time.Millisecond},
		"step":  {Kind: "pub", Origin: "agent", From: "pipeline", To: "pipeline on :pipeline/step", Args: `[{:step :dev}]`},
		"eval":  {Kind: "eval", Origin: "agent", From: "agent/3", Args: `["(file/write \"x\" \"y\")"]`, Reply: `(ok :done)`, Gate: "free"},
		"note":  {Kind: "note", Note: ":gh restarted: gh.puff:14:9"},
	}
	marks := map[string]string{"plan/draft": "write", "plan/board": "read", "gh/pr": "write", "todo/add": "write", "todo/done": "write"}
	for _, tt := range []struct{ q, want string }{
		{"", "board draft err eval fault held note step"},
		{"to:plan", "board draft"},
		{"to::plan/draft", "draft"},
		{"to:pipeline", "step"},
		{"to:pipeline/step", ""}, // a topic is to:pipeline only
		{"from:agent", "draft eval held"},
		{"from:client", "board err"},
		{"origin:user", "board err"},
		{"kind:pub", "step"},
		{"kind:err", "err fault"}, // a fault is an error too; what waits for review isn't
		{"kind:fault", "fault"},
		{"kind:pending", "held"},
		{"kind:ok", "board draft eval"},
		{"time:>500ms", "draft"},
		{"time:<5ms", "board err"}, // what took no time has none
		{"time:>1ms", "draft fault board"},
		{"mark:write", "draft err eval fault held"},
		{"mark:read", "board"},
		{"gate:held", "held"},
		{"project:tally", "draft held"},
		{"project:tal", ""}, // a project is named whole
		{"-project:tally kind:call", "board err fault"},
		{"gate:for", "fault"},
		{"-gate:for kind:err", "err"},
		{"-to:gh -to:pipeline from:agent/3", "draft eval held"},
		{"GH.PUFF", "fault note"},
		{"to:plan fix", "draft"},
	} {
		q, err := ParseQuery(tt.q)
		if err != nil {
			t.Errorf("%q: %v", tt.q, err)
			continue
		}
		var got []string
		for _, name := range []string{"board", "draft", "err", "eval", "fault", "held", "note", "step"} {
			if q.Match(es[name], marks) {
				got = append(got, name)
			}
		}
		want := strings.Fields(tt.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%q matches %v, want %v", tt.q, got, want)
		}
	}
}

func TestQueryErrors(t *testing.T) {
	for _, tt := range []struct{ q, want string }{
		{"kind:big", "kind:big: want kind:"},
		{"time:500ms", "time:500ms: want time:>500ms"},
		{"time:>soon", "time:>soon: want time:>500ms"},
		{"origin:bot", "origin:bot: want origin:user"},
		{"mark:yes", "mark:yes: want mark:read"},
		{"color:red", "color:red: no such filter"},
		{"to:", "to:: want something after to:"},
	} {
		_, err := ParseQuery(tt.q)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%q: %v, want %s...", tt.q, err, tt.want)
		}
	}
	if q, _ := ParseQuery("mark:write"); !q.Marks() {
		t.Error("mark:write doesn't need marks")
	}
	if q, _ := ParseQuery("  "); !q.Empty() {
		t.Error("blank isn't empty")
	}
}
