package daemon

import (
	"strings"
	"testing"
)

func TestViews(t *testing.T) {
	dir := tempDir(t)
	start(t, dir)
	c := dial(t, dir)
	tests := []struct{ src, want string }{
		{`(view/items [{:label "a" :detail "d" :action [:todo/done 1]} {:label "b" :extra 1}])`,
			`(items [{:action [:todo/done 1] :detail "d" :label "a"} {:extra 1 :label "b"}])`},
		{`(view/items [])`, `(items [])`},
		{`(view/text "hi")`, `(text "hi")`},
		{`(view/locations [{:file "a.go" :line 3 :col 1 :note "x"} {:file "b.go"}])`,
			`(locations [{:col 1 :file "a.go" :line 3 :note "x"} {:file "b.go"}])`},
		{`(view/items [{:detail "d"}])`, `has no :label`},
		{`(view/items [{:label 1}])`, `want :label as a string, got 1`},
		{`(view/items [{:label "a" :action ["todo/done" 1]}])`, `want :action as a message like [:todo/done 12]`},
		{`(view/items [{:label "a" :actions [{:label "Tick it" :action [:todo/done 1]} {:label "Drop it" :action [:todo/drop 1]}]}])`,
			`(items [{:actions [{:action [:todo/done 1] :label "Tick it"} {:action [:todo/drop 1] :label "Drop it"}] :label "a"}])`},
		{`(view/items [{:label "a" :actions [{:label "Tick it"}]}])`, `want :actions as a vector of {:label "..." :action [:address/fn args...]} ({:label "Tick it"} has no :action)`},
		{`(view/items [{:label "a" :actions [{:label :tick :action [:todo/done 1]}]}])`, `(want :label as a string, got :tick)`},
		{`(view/items {:label "a"})`, `want a vector of maps`},
		{`(view/text 1)`, `want a string, got 1`},
		{`(view/locations [{:file "a.go" :line 0}])`, `want :line as an int from 1, got 0`},
		{`(view/items [{:label "a"}] {:changed :todo/changed})`, `(items [{:label "a"}] {:changed :todo/changed})`},
		{`(view/text "hi" {:changed :x/y})`, `(text "hi" {:changed :x/y})`},
		{`(view/board [] {:changed :plan/changed})`, `(board [] {:changed :plan/changed})`},
		{`(view/items [] {:changed "todo/changed"})`, `want :changed as a topic, like :plan/changed, got "todo/changed"`},
		{`(view/items [] {:refresh 1s})`, `opts have no :refresh`},
		{`(view/items [] [:changed])`, `want opts as a map`},
		{`(view/board [{:title "Now" :items [{:label "a" :action [:todo/done 1]}]} {:title "Later" :items []}])`,
			`(board [{:items [{:action [:todo/done 1] :label "a"}] :title "Now"} {:items [] :title "Later"}])`},
		{`(view/board [{:title "Now"}])`, `has no :items`},
		{`(view/board [{:items []}])`, `has no :title`},
		{`(view/board [{:title "Now" :items [{:label 1}]}])`, `column "Now": want :label as a string, got 1`},
		{`(view/board [{:title "Now" :items {}}])`, `column "Now": want a vector of maps`},
	}
	for _, tt := range tests {
		resp, err := c.Eval(tt.src, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Value + resp.Error; !strings.Contains(got, tt.want) {
			t.Errorf("%s = %s, want %s", tt.src, got, tt.want)
		}
	}
}
