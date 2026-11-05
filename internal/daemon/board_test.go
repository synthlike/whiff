package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lead plans its runs on services/board.puff and builds them there: a
// run's ask still says whose work it is, the ticket follows its run, and a
// project has at most 4 going.
func TestBoardBuild(t *testing.T) {
	dir := tempDir(t)
	repo := filepath.Join(dir, "tally")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	board, err := os.ReadFile(filepath.Join("..", "..", "services", "board.puff"))
	if err != nil {
		t.Fatal(err)
	}
	writeServices(t, dir, map[string]string{
		"board": string(board),
		// A pipeline whose runs start and never end, each saying whose
		// build it was.
		"pipeline": `(service pipeline :state {:runs []}
  (ask (build repo base task check model) "Starts a run.")
  (on (whiff/asked state id) :update "Starts the run asked for."
    (match (vector/find (inbox :build) (fn ({:id i}) (= i id)))
      [(some m)
       (let [rs (map/get-or state :runs [])
             [repo base task check model] (map/get-or m :args [])]
         (ok (map/set state :runs (vector/append rs {:run (+ 1 (vector/length rs)) :task task :step :dev :round 1 :branch "b"
                                                     :dir repo :from (map/get-or m :from :you) :session (map/get-or m :session :none)}))))]
      [_ (ok state)]))
  (on (runs state) :read "Every run." (ok (map/get-or state :runs []))))`,
	})
	start(t, dir)
	you := dial(t, dir)
	if _, err := you.Sub("review/pending"); err != nil {
		t.Fatal(err)
	}
	lead := dial(t, dir)
	lead.Agent = true
	if r, err := lead.Claim("agent", `(ask (tell text) "Asks the lead.")`); err != nil || r.Value != "(ok :agent/1)" {
		t.Fatalf("claim = %+v %v", r, err)
	}
	if r, err := you.Mode("agent/1", "trusted", []string{"board", "pipeline"}, time.Hour); err != nil || r.Value != "(ok :done)" {
		t.Fatalf("trust = %+v %v", r, err)
	}
	call := func(target, args string) string {
		t.Helper()
		r, err := lead.Message("call", target, args, "")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		return r.Value
	}
	q := func(s string) string { return `"` + s + `"` }

	for n, title := range []string{"Scaffold", "Part a", "Part b", "Part c", "Integration"} {
		if got, want := call("board/plan", "["+q(repo)+" "+q(title)+" "+q("the task of "+title)+"]"), "(ok "+string(rune('1'+n))+")"; got != want {
			t.Fatalf("plan %s = %s, want %s", title, got, want)
		}
	}
	if got := call("board/build", `[1 "main" ["true"] "haiku"]`); !strings.Contains(got, ":kind :pending") {
		t.Fatalf("build = %s, want pending", got)
	}
	runs := func() string {
		r, err := you.Eval(`(call :pipeline/runs)`, dir)
		if err != nil {
			t.Fatal(err)
		}
		return r.Value
	}
	eventually(t, "the run, the lead's", func() string {
		if strings.Contains(runs(), ":session :agent/1") {
			return "the lead's"
		}
		return runs()
	}, "the lead's")

	// The ticket is at its run's step, with its run, and the run isn't
	// shown again beside it.
	eventually(t, "the ticket in Doing", func() string {
		r, _ := you.Eval(`(call :board/for "tally")`, dir)
		v := r.Value + r.Error
		// A column prints its :items before its :title.
		i, j := strings.Index(v, `:title "Todo"}`), strings.Index(v, `:title "Doing"}`)
		if i < 0 || j < i {
			return v
		}
		doing := v[i:j]
		if strings.Count(doing, ":detail") == 1 && strings.Contains(doing, `:label "Scaffold"`) && strings.Contains(doing, "#1 · haiku dev · run 1, round 1, b") {
			return "Scaffold"
		}
		return v
	}, "Scaffold")

	for id := 2; id <= 4; id++ {
		if got := call("board/build", "["+string(rune('0'+id))+` "b" ["true"] ""]`); !strings.Contains(got, ":kind :pending") {
			t.Fatalf("build %d = %s, want pending", id, got)
		}
	}
	eventually(t, "4 runs going", func() string { return string(rune('0' + strings.Count(runs(), ":step :dev"))) }, "4")
	if got := call("board/build", `[5 "b" ["true"] ""]`); !strings.Contains(got, ":kind :busy") || !strings.Contains(got, "tally has 4 runs going") {
		t.Errorf("a 5th build = %s, want busy", got)
	}
	// One add made has no task to build.
	if got, _ := you.Eval(`[(call :board/add "tally" "by hand") (call :board/build 6 "b" ["true"] "")]`, dir); !strings.Contains(got.Value, ":kind :no-ticket") {
		t.Errorf("building one by hand = %s", got.Value)
	}
}
