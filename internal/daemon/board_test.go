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
	t.Parallel()
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
                                                     :dir repo :from (map/get-or m :from :you) :session (map/get-or m :session :none)
                                                     :model model :cost 1.0 :started (some (time/now))}))))]
      [_ (ok state)]))
  (on (runs state) :read "Every run." (ok (map/get-or state :runs []))))`,
		// The team's lead, as an agent's usage says what its turns spent on
		// each project.
		"lead": `(service lead
  (on (usage) :read "What its turns spent." (ok {:spent 0.7 :turns 2 :day 10.0 :projects {"tally" 0.5 "shorty" 0.2}}))
  (on (tell text) "Says the project its tell is in." (ok (project/name))))`,
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
	// tally's team: runs may use haiku or sonnet, reviewed on sonnet.
	team := func(changes string) {
		t.Helper()
		if r, err := you.Eval(`(call :board/set-team "tally" `+changes+`)`, dir); err != nil || !strings.HasPrefix(r.Value, "(ok {") {
			t.Fatalf("set-team %s = %+v %v", changes, r, err)
		}
	}
	team(`{:models ["haiku" "sonnet"] :reviewer "sonnet"}`)
	if got := call("board/build", `[1 "main" ["true"] "opus"]`); !strings.Contains(got, ":kind :not-allowed") {
		t.Errorf("build on opus = %s, want not allowed", got)
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
	eventually(t, "the run, the lead's, on the team's reviewer", func() string {
		if strings.Contains(runs(), ":session :agent/1") && strings.Contains(runs(), `:model {:dev "haiku" :reviewer "sonnet"}`) {
			return "the lead's"
		}
		return runs()
	}, "the lead's")
	team(`{:reviewer :default}`)

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
	// More may go, but they and the lead's turns for tally cost $4.50
	// today, of $3 a day.
	// As sniff's Change sends it, printed.
	team(`"{:most 10 :day 3}"`)
	if got := call("board/build", `[5 "b" ["true"] ""]`); !strings.Contains(got, ":kind :over-budget") {
		t.Errorf("a build over budget = %s", got)
	}
	if r, _ := you.Eval(`(call :board/team "tally")`, dir); !strings.Contains(r.Value, ":going 4") || !strings.Contains(r.Value, ":spent 4.5") || !strings.Contains(r.Value, ":most 10") {
		t.Errorf("tally's team = %s", r.Value)
	}
	// Telling tally's lead puts its turns in tally.
	if r, _ := you.Eval(`(call :board/tell "tally" "build it")`, dir); r.Value != `(ok (some "tally"))` {
		t.Errorf("tell = %s %s", r.Value, r.Error)
	}
	// One add made has no task to build.
	if got, _ := you.Eval(`[(call :board/add "tally" "by hand") (call :board/build 6 "b" ["true"] "")]`, dir); !strings.Contains(got.Value, ":kind :no-ticket") {
		t.Errorf("building one by hand = %s", got.Value)
	}
}
