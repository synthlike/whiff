package main

import (
	"slices"
	"strings"
	"testing"
)

// drawn is printed, a reply, as sniff draws it, with marks.
func drawn(t *testing.T, printed string, marks map[string]string) string {
	t.Helper()
	var b strings.Builder
	if err := pages.ExecuteTemplate(&b, "shown", show(printed, marks, "/view/x/y")); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRender(t *testing.T) {
	marks := map[string]string{"p/show": "read", "p/retry": "write", "p/go": "write"}
	items := `(ok (items [{:label "Fix <b>it</b>\n\n1. one ` + "`a`" + `\n2. two" :detail "d"
	  :actions [{:label "Show" :action [:p/show 2]} {:label "Retry" :action [:p/retry 2] :ask "Why?"} {:label "Go" :action [:p/go]}]}
	  {:label "plain" :action [:p/go "x"]}] {:changed :p/changed}))`
	got := drawn(t, items, marks)
	for _, want := range []string{
		// Text is escaped, and long text is prose.
		`<p>Fix &lt;b&gt;it&lt;/b&gt;</p><ol start="1"><li>one <code>a</code></li><li>two</li></ol>`,
		`<span class="detail">d</span>`,
		// A :read action links to its page.
		`<a class="action link labelled" href="/view/p/show?args=%5B2%5D"`,
		// One that asks opens a box for the answer, then posts.
		`<details class="asks"><summary class="action labelled"`,
		`<input name="answer" placeholder="Why?"`,
		// A :write one is a form that shows its target and goes back here.
		`<input type="hidden" name="action" value="[:p/go]"><input type="hidden" name="back" value="/view/x/y"><button class="action labelled" title="sends [:p/go] as you">Go<span class="to"> → :p/go</span></button>`,
		// An unlabelled :action is labelled with its message.
		`<button class="action" title="sends [:p/go &#34;x&#34;] as you">[:p/go &#34;x&#34;]</button>`,
		// The reply, folded as puff, under it.
		`<details class="raw"><summary>as puff</summary>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("items lack\n%s\nin\n%s", want, got)
		}
	}
	if strings.Contains(got, "<b>") {
		t.Errorf("items let markup through:\n%s", got)
	}
	if v := show(items, marks, "/").View; v == nil || !slices.Equal(v.Topics, []string{"p/changed"}) {
		t.Errorf("topics = %+v", v)
	}

	for _, tt := range []struct{ printed, want string }{
		{`(ok (board [{:title "Done" :items []} {:title "Dev" :items [{:label "a"}]}]))`,
			`<div class="view board"><div class="column"><h4>Done<span class="count">0</span></h4><p class="hint">None.</p></div><div class="column"><h4>Dev<span class="count">1</span></h4><div class="item"><span>a</span></div></div></div>`},
		{`(ok (text "a <diff>"))`, `<div class="view"><pre>a &lt;diff&gt;</pre></div>`},
		{`(ok (locations [{:file "a.go" :line 3 :col 4 :note "here"}]))`, `data-copy="a.go:3:4"`},
		// A reply that holds no view is folded as puff.
		{`(ok [1 :a "b"])`, `<div class="value"><span>(<span class="tag">ok</span> <span>[<span class="lit">1</span> <span class="kw">:a</span> <span class="str">&#34;b&#34;</span>]</span>)</span></div>`},
		// A view of a shape sniff doesn't know, too.
		{`(ok (items [{:detail "no label"}]))`, `<div class="value">`},
	} {
		if got := drawn(t, tt.printed, marks); !strings.Contains(got, tt.want) {
			t.Errorf("%s drew\n%s\nwant it to have\n%s", tt.printed, got, tt.want)
		}
	}
}

func TestProse(t *testing.T) {
	p := proseOf("Run:\n```sh\nwhiff test x\n```\n- a\n- b `c`")
	want := []block{
		{Tag: "p", Lines: [][]span{{{Text: "Run:"}}}},
		{Tag: "pre", Code: "whiff test x"},
		{Tag: "ul", Lines: [][]span{{{Text: "a"}}, {{Text: "b "}, {Text: "c", Code: true}}}},
	}
	if p.Plain || len(p.Blocks) != len(want) {
		t.Fatalf("prose = %+v", p)
	}
	for i := range want {
		if got := p.Blocks[i]; got.Tag != want[i].Tag || got.Code != want[i].Code || !slices.EqualFunc(got.Lines, want[i].Lines, slices.Equal) {
			t.Errorf("block %d = %+v, want %+v", i, got, want[i])
		}
	}
	if p := proseOf("one line"); !p.Plain || p.Text != "one line" {
		t.Errorf("one line = %+v", p)
	}
}
