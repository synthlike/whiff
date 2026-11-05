package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// projectOf is the project of work in dir: the name of the git repository
// dir is in, tally for ~/src/tally/cmd; a worktree's is its repository's,
// as git worktree add makes them, so a pipeline run's is the project it is
// for. "" outside a repository, or for one at your home directory or
// above it, as a dotfiles repository is no project.
func projectOf(dir string) string {
	if dir == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	for d := filepath.Clean(dir); d != home && d != filepath.Dir(d); d = filepath.Dir(d) {
		dot := filepath.Join(d, ".git")
		fi, err := os.Lstat(dot)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			return filepath.Base(d)
		}
		// A worktree's .git is a file: gitdir: <repo>/.git/worktrees/<name>.
		b, err := os.ReadFile(dot)
		gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if err != nil || !ok {
			return filepath.Base(d)
		}
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(d, gitdir)
		}
		if repo, _, ok := strings.Cut(filepath.ToSlash(gitdir), "/.git/worktrees/"); ok {
			return filepath.Base(repo)
		}
		return filepath.Base(d)
	}
	return ""
}
