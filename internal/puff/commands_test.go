package puff

import (
	"strings"
	"testing"
)

func TestReadOnlyCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		read bool
	}{
		{"git status", true},
		{"git status --porcelain=v2 --branch", true},
		{"git status -sb", false}, // flags run together aren't read
		{"git -c core.quotePath=false status", true},
		{"git -c core.fsmonitor=x status", false},
		{"git -C /tmp --no-pager log --oneline -n 5", true},
		{"git log -n --output=x", false}, // a value can't be a flag
		{"git log --output=x", false},
		{"git log main..HEAD -- go.mod", true},
		{"git diff -- --output=x", true}, // after --, a path
		{"git diff --ext-diff", false},
		{"git branch", true},
		{"git branch --show-current", true},
		{"git branch -a", true},
		{"git branch new", false},
		{"git branch -d old", false},
		{"git branch -D old", false},
		{"git remote -v", true},
		{"git remote add origin x", false},
		{"git remote get-url origin", true},
		{"git tag -l", true},
		{"git tag v1", false},
		{"git stash list", true},
		{"git stash", false},
		{"git push", false},
		{"git", false},
		{"git statusx", false},
		{"/usr/bin/git status", false},
		{"gh pr list", true},
		{"gh pr list --json number,title --state open", true},
		{"gh pr list --json=number", true},
		{"gh pr list --web", false},
		{"gh pr list 12", false},
		{"gh pr checks 12 --json name,bucket", true},
		{"gh pr create", false},
		{"gh pr merge 12", false},
		{"gh api repos/a/b/pulls", true},
		{"gh api -X GET repos/a/b", true},
		{"gh api --method=GET repos/a/b", true},
		{"gh api -X POST repos/a/b", false},
		{"gh api --method=POST repos/a/b", false},
		{"gh api repos/a/b -f title=x", false},
		{"gh api graphql --input q.json", false},
		{"go vet ./...", true},
		{"go vet -vettool=x ./...", false},
		{"go env GOPATH", true},
		{"go env -w GOPATH=x", false},
		{"go test ./...", false},
		{"go list -m all", true},
		{"go list -m -u all", false}, // asks the module proxy
		{"go list -m -versions x.org/m", false},
		{"go list -m x.org/secret@latest", false}, // a query asks it too
		{"pwd", true},
		{"pwd x", false},
		{"ls -la", true},
		{"head -n 5 a.go", true},
		{"rm -rf x", false},
	}
	for _, tt := range tests {
		var argv []Value
		for _, s := range strings.Fields(tt.cmd) {
			argv = append(argv, String(s))
		}
		if got := ReadOnlyCommand(NewVector(argv...)); got != tt.read {
			t.Errorf("ReadOnlyCommand(%s) = %v, want %v", tt.cmd, got, tt.read)
		}
	}
	for _, v := range []Value{NewVector(), NewVector(String("git"), Int(1)), String("git status")} {
		if ReadOnlyCommand(v) {
			t.Errorf("ReadOnlyCommand(%s) = true, want false", Print(v))
		}
	}
}
