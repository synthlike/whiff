package main

import (
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// A lead's work, as whiff services lead lists it.
func TestWorkLine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ in, want string }{
		{`{:id 7 :to :pipeline :status :ended :at 2026-11-03T11:57:00Z :result (ok {:run 7 :status :ready :branch "whiff/7" :cost 0.314})}`,
			"7    :pipeline  ready whiff/7, $0.31, 3m ago"},
		{`{:id 12 :to :helper :status :ended :at 2026-11-03T11:00:00Z :result (ok "sum: 3")}`, "12   :helper  sum: 3, 1h ago"},
		{`{:id 13 :to :pipeline :status :ended :at 2026-11-03T11:59:00Z :result (err {:kind :exit :message "git exited 1"})}`,
			"13   :pipeline  failed: git exited 1, 1m ago"},
		{`{:id 140 :to :pipeline :fn :build :status :waiting :at 2026-11-03T11:59:30Z}`, "140  :pipeline/build  waiting, 30s ago"},
		{`{:id 141 :to :notes :fn :add :status :held :at 2026-11-03T11:59:30Z}`, "141  :notes/add  held for your review, 30s ago"},
		{`{:id 142 :to :pipeline :fn :build :status :waiting :why "its work runs: 2 tasks open" :at 2026-11-03T11:59:30Z}`, "142  :pipeline/build  its work runs: 2 tasks open, 30s ago"},
	} {
		v, err := puff.ReadValue(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := workLine(v.(puff.Map), 3, now); got != tt.want {
			t.Errorf("workLine %s =\n%q, want\n%q", tt.in, got, tt.want)
		}
	}
}
