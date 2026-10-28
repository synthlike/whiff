package daemon

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

var update = flag.Bool("update", false, "rewrite docs/stdlib.md from the function tables")

// TestStdlibDoc checks that docs/stdlib.md is the function tables as they
// are; go test ./internal/daemon -run TestStdlibDoc -update rewrites it.
func TestStdlibDoc(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "stdlib.md")
	want := stdlibDoc(puff.Base(), (&daemon{}).builtins(&caller{}))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Error("docs/stdlib.md is out of date: go test ./internal/daemon -run TestStdlibDoc -update")
	}
}

const stdlibIntro = `# puff's functions

Every function puff has, generated from its tables by
` + "`go test ./internal/daemon -run TestStdlibDoc -update`" + `; don't edit it by
hand. In a REPL, ` + "`(help)`" + ` lists them and ` + "`(help string/split)`" + ` shows one.

A signature names the arguments: ` + "`[b]`" + ` is optional and ` + "`...`" + ` takes any
number more of the one before it. Functions are ` + "`:read`" + ` unless marked
` + "`:write`" + `: a ` + "`:write`" + ` function changes something outside puff (a file, a
process, a message sent). ` + "`whiff ls`" + ` shows each handler's mark, worked out
from what it calls; whiff's review gate will go by it. A handler marked
` + "`:read`" + ` that calls a ` + "`:write`" + ` function is an override, which ` + "`whiff ls`" + ` lists;
any other ` + "`:read`" + ` handler that reaches a ` + "`:write`" + ` function at run time faults.

Failures a caller should handle are values: ` + "`(err {:kind :message})`" + `, or
` + "`(none)`" + ` for a lookup that finds nothing. A wrong argument, such as a string
where an int goes, is a fault instead and stops the program.

The language's own forms, ` + "`def let fn if cond do and or match when loop recur -> try catch test`" + `,
aren't functions and aren't listed here.
`

// stdlibDoc is docs/stdlib.md: puff's base table, by namespace, then
// whiff's functions.
func stdlibDoc(base, whiff []*puff.Builtin) string {
	var b strings.Builder
	b.WriteString(stdlibIntro)
	b.WriteString("\n## puff\n\nIn `puff` and in whiff alike. Names without a namespace are under core.\n")
	var order []string
	groups := map[string][]*puff.Builtin{}
	for _, f := range base {
		ns, _, ok := strings.Cut(f.Name, "/")
		if !ok {
			ns = "core"
		}
		if groups[ns] == nil {
			order = append(order, ns)
		}
		groups[ns] = append(groups[ns], f)
	}
	for _, ns := range order {
		b.WriteString("\n### " + ns + "\n\n")
		entries(&b, groups[ns])
	}
	b.WriteString("\n## whiff\n\nOnly in whiff: in services, `whiff eval` and `whiff repl`. In a handler,\n`call`, `send` or `send-after` with a literal target counts as that target's\nmark rather than `:write`, and `publish` to a literal topic as its subscribers'.\n\n")
	entries(&b, whiff)
	return b.String()
}

func entries(b *strings.Builder, fs []*puff.Builtin) {
	for _, f := range fs {
		b.WriteString("- `" + f.Signature() + "`")
		if f.Mark == puff.MarkWrite {
			b.WriteString(" `:write`")
		}
		b.WriteString(": " + f.Doc + "\n")
	}
}
