package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectOf(t *testing.T) {
	root := tempDir(t)
	repo := filepath.Join(root, "tally")
	tree := filepath.Join(root, "runs", "tally-7")
	odd := filepath.Join(root, "odd")
	for _, d := range []string{filepath.Join(repo, ".git", "worktrees", "tally-7"), filepath.Join(repo, "cmd", "tally"), tree, odd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(tree, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "tally-7")+"\n")
	write(filepath.Join(odd, ".git"), "not a gitdir line")
	for dir, want := range map[string]string{
		repo:                                "tally",
		filepath.Join(repo, "cmd", "tally"): "tally",
		tree:                                "tally", // a worktree is its repository's
		odd:                                 "odd",
		root:                                "",
		"":                                  "",
		"/":                                 "",
	} {
		if got := projectOf(dir); got != want {
			t.Errorf("projectOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A job run in a repository puts its chain in that project: the answer
// and what its service does on it carry the tag, and history finds them
// by project:.
func TestProjectTag(t *testing.T) {
	dir := tempDir(t)
	repo := filepath.Join(dir, "tally")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeServices(t, dir, map[string]string{
		"builder": `(service builder
  (on (build) "Runs a job in tally." (call :jobs/run ["true"] {:dir "` + repo + `"}))
  (on (whiff/done id r) "Tells after on it." (send :after/it 1)))`,
		"after": `(service after (on (it x) "Says its chain's project, and tally's." (ok [(project/name) (project/name "` + repo + `/cmd")])))`,
	})
	start(t, dir)
	c := dial(t, dir)
	if resp, err := c.Eval(`(call :builder/build)`, dir); err != nil || resp.Error != "" {
		t.Fatalf("build: %v %s", err, resp.Error)
	}
	eventually(t, "the tagged send", func() string {
		h, err := historyOf(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(h, "\n") {
			if strings.Contains(line, ":builder -> :after/it 1") {
				_, tag, _ := strings.Cut(line, "  #")
				return tag
			}
		}
		return "no send yet"
	}, "tally")

	// A handler knows its chain's project, and that of a directory.
	historyHas(t, c, `:builder -> :after/it 1  (ok [(some "tally") (some "tally")])`)
	if resp, err := c.Eval(`[(project/name) (project/name "/")]`, dir); err != nil || resp.Value != `[(none) (none)]` {
		t.Errorf("project outside one = %s %v", resp.Value, err)
	}

	h, err := historyOf(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(h), "\n") {
		tagged := strings.HasSuffix(line, "#tally")
		// The request began before the job named a project; what came of
		// the job is in it.
		want := strings.Contains(line, ":jobs -> :builder") || strings.Contains(line, ":after/it")
		if tagged != want {
			t.Errorf("tagged %v, want %v: %s", tagged, want, line)
		}
	}
}
