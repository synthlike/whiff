package daemon

import (
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

// A task's result, cut down for a line about it: a run's keeps its run,
// status, branch and cost, not its long note; an err keeps its message's
// first line.
func TestBrief(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("review ", 40)
	for _, tt := range []struct{ in, want string }{
		{`(ok {:run 7 :status :ready :branch "whiff/7" :cost 0.31 :note "` + long + `"})`, `(ok {:branch "whiff/7" :cost 0.31 :run 7 :status :ready})`},
		{`(err {:kind :exit :message "git exited 1\nand more" :stderr "` + long + `"})`, `(err {:kind :exit :message "git exited 1"})`},
		{`(ok "two\nlines")`, `(ok "two")`},
		{`(ok :done)`, `(ok :done)`},
		{`(ok [1 2])`, `(ok "[1 2]")`},
	} {
		v, err := puff.ReadValue(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := puff.Print(brief(v)); got != tt.want {
			t.Errorf("brief %s = %s, want %s", tt.in, got, tt.want)
		}
	}
}
