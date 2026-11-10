package puff

import (
	"slices"
	"strings"
)

// readOnlyCommands is the table of commands that only read: proc/run,
// proc/output and :jobs/run of one are :read, not :write. Each is a program
// and its subcommand, the flags it may have, and whether it takes other
// arguments (paths, revisions, a pull request's number). A command matches
// if it is the program, any of the program's commandGlobals, the
// subcommand, then only flags listed and, if it takes them, other
// arguments; anything else, an unlisted flag or one run together with
// another (-sb), is :write.
//
// A flag is "-s"; "--format=" for --format= and anything after it; "-n _"
// for -n and a value, which mustn't start with -, and for --name=value if
// the name starts with --; or "-X GET" for -X and that value exactly.
//
// It narrows the trust rather than removing it: a program's config and
// hooks (git's core.fsmonitor or a diff driver) or a different binary of
// the same name earlier on PATH can still write. Review a change here as
// you would code: the review gate relies on it.
var readOnlyCommands = []command{
	{"git status", true, "-s, --short, -b, --branch, --porcelain, --porcelain=, --long, -z, -u, --untracked-files=, --ignored, --ignored=, --show-stash, --ahead-behind, --no-ahead-behind, -v, --verbose"},
	{"git log", true, "--oneline, -n _, --max-count=, --skip=, --format=, --pretty=, --graph, --decorate, --decorate=, --all, --stat, --shortstat, --name-only, --name-status, -p, --patch, --no-merges, --merges, --first-parent, --reverse, --since=, --until=, --after=, --before=, --author=, --grep=, -i, --follow, --date=, -z, --abbrev-commit, --no-color"},
	{"git diff", true, "--stat, --shortstat, --numstat, --name-only, --name-status, --cached, --staged, --unified=, --no-color, -z, --check, --exit-code, --quiet, -w, --ignore-all-space, --word-diff, --diff-filter=, --merge-base"},
	{"git show", true, "--stat, --name-only, --name-status, --format=, --pretty=, --oneline, -s, --no-patch, --no-color, --abbrev-commit"},
	{"git branch", false, "--show-current, -a, --all, -r, --remotes, -v, -vv, --verbose, --list, --merged, --no-merged, --format=, --sort=, --no-color"},
	{"git rev-parse", true, "--show-toplevel, --show-prefix, --git-dir, --is-inside-work-tree, --abbrev-ref, --symbolic-full-name, --verify, -q, --quiet, --short, --short="},
	{"git rev-list", true, "--count, --all, -n _, --max-count=, --left-right, --first-parent, --no-merges, --reverse, --since=, --until="},
	{"git ls-files", true, "-c, --cached, -m, --modified, -o, --others, -d, --deleted, -s, --stage, --exclude-standard, --full-name, -z"},
	{"git merge-base", true, "--is-ancestor, --all"},
	{"git describe", true, "--tags, --always, --abbrev=, --exact-match"},
	{"git blame", true, "-L _, -w, -M, -s, -e, --porcelain, --line-porcelain"},
	{"git remote", false, "-v, --verbose"},
	{"git remote get-url", true, "--all, --push"},
	{"git tag", false, "-l, --list, --sort=, --contains _, --points-at _"},
	{"git stash list", false, ""},

	{"gh pr list", false, ghRead + ", -s _, --state _, -L _, --limit _, -B _, --base _, -H _, --head _, -A _, --author _, -a _, --assignee _, -l _, --label _, -S _, --search _, -d, --draft"},
	{"gh pr view", true, ghRead + ", -c, --comments"},
	{"gh pr diff", true, "-R _, --repo _, --name-only, --patch, --color _"},
	{"gh pr checks", true, ghRead + ", --required"},
	{"gh pr status", false, ghRead + ", -c, --conflict-status"},
	{"gh issue list", false, ghRead + ", -s _, --state _, -L _, --limit _, -A _, --author _, -a _, --assignee _, -l _, --label _, -S _, --search _"},
	{"gh issue view", true, ghRead + ", -c, --comments"},
	{"gh run list", false, ghRead + ", -L _, --limit _, -b _, --branch _, -w _, --workflow _, -s _, --status _, -c _, --commit _, -e _, --event _, -u _, --user _"},
	{"gh run view", true, ghRead + ", --log, --log-failed, -v, --verbose, -j _, --job _, --attempt _, --exit-status"},
	{"gh repo view", true, "--json _, -q _, --jq _, -t _, --template _, -b _, --branch _"},
	// Without -f, -F or --input, gh api sends a GET.
	{"gh api", true, "-q _, --jq _, -t _, --template _, --paginate, --slurp, -i, --include, --silent, --cache _, -X GET, --method GET"},

	{"go vet", true, "-json"},
	// Not -u nor -versions, nor a module query (path@version, refused
	// below): they ask the module proxy, so a path could carry data out.
	{"go list", true, "-m, -json, -json=, -f _, -e, -deps, -test, -find"},
	{"go doc", true, "-all, -src, -short, -u, -c, -cmd"},
	{"go version", true, "-m, -v"},
	{"go env", true, "-json"},

	{"pwd", false, ""},
	{"ls", true, "-l, -a, -la, -al, -1, -h, -lh, -R"},
	{"cat", true, ""},
	{"head", true, "-n _"},
	{"wc", true, "-l, -w, -c"},
}

// ghRead is the flags gh's list and view commands share.
const ghRead = "--json _, -q _, --jq _, -t _, --template _, -R _, --repo _"

// commandGlobals is the flags a program takes before its subcommand.
var commandGlobals = map[string]string{
	"git": "-C _, --no-pager, -c core.quotePath=false",
}

type command struct {
	name  string // the program and subcommand: "git status"
	args  bool   // whether it takes arguments besides its flags
	flags string // its flags, separated by ", "
}

// ReadOnlyCommand says whether cmd, a vector of the program and its
// arguments as proc/run takes it, is in the table of commands that only
// read.
func ReadOnlyCommand(cmd Value) bool {
	v, ok := cmd.(Vector)
	if !ok || v.Len() == 0 {
		return false
	}
	argv, err := strs(v.items)
	if err != nil {
		return false
	}
	rest := argv[1:]
	if argv[0] == "go" && slices.ContainsFunc(rest, func(a string) bool { return strings.Contains(a, "@") }) {
		return false
	}
	globals := strings.Split(commandGlobals[argv[0]], ", ")
	for len(rest) > 0 {
		n := flagLen(rest, globals)
		if n == 0 {
			break
		}
		rest = rest[n:]
	}
	for _, c := range readOnlyCommands {
		words := strings.Fields(c.name)
		sub := words[1:]
		if words[0] == argv[0] && len(rest) >= len(sub) && slices.Equal(rest[:len(sub)], sub) && c.matches(rest[len(sub):]) {
			return true
		}
	}
	return false
}

// ReadOnlyCommands names the commands in the table, "git status" and so on.
func ReadOnlyCommands() []string {
	names := make([]string, len(readOnlyCommands))
	for i, c := range readOnlyCommands {
		names[i] = c.name
	}
	return names
}

// matches says whether args, what follows c's subcommand, are only c's flags
// and, if c takes them, other arguments. After --, everything is one.
func (c command) matches(args []string) bool {
	flags := strings.Split(c.flags, ", ")
	for i := 0; i < len(args); {
		switch a := args[i]; {
		case a == "--":
			return c.args || i == len(args)-1
		case strings.HasPrefix(a, "-") && a != "-":
			n := flagLen(args[i:], flags)
			if n == 0 {
				return false
			}
			i += n
		case !c.args:
			return false
		default:
			i++
		}
	}
	return true
}

// flagLen is how many of args the flag args[0] takes, if it is one of
// flags: 1, 2 with its value, or 0 if it isn't one.
func flagLen(args []string, flags []string) int {
	for _, f := range flags {
		name, val, two := strings.Cut(f, " ")
		switch {
		case f == "":
		case !two && strings.HasSuffix(name, "=") && strings.HasPrefix(args[0], name):
			return 1
		case !two && args[0] == name:
			return 1
		case two && args[0] == name && len(args) > 1 && (val == "_" && !strings.HasPrefix(args[1], "-") || args[1] == val):
			return 2
		case two && strings.HasPrefix(name, "--") && strings.HasPrefix(args[0], name+"="):
			if v := strings.TrimPrefix(args[0], name+"="); val == "_" || v == val {
				return 1
			}
		}
	}
	return 0
}
