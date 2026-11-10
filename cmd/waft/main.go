// Command waft provisions Ubuntu hosts over ssh from a puff file that
// describes them: plan shows what differs, apply changes the host to match.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

const usage = `usage: waft plan file.puff    show what would change on each host
       waft apply file.puff   change each host to match, then check again

The file is puff whose last form is a host, or a vector of hosts. A host
is its ssh dest, an admin with passwordless sudo, and its resources:

  (use "whiff.puff")
  (host "admin@203.0.113.7"
    [(pkg "git" "golang-go")
     (user "whiff" {:keys ["~/.ssh/id_ed25519.pub"] :linger #t}
       [(ssh-key)
        (file "~/.gitconfig" {:from "dotfiles/gitconfig"})
        (repo "~/src/mdtoc" "git@github.com:me/mdtoc.git")
        (whiff {:version "v0.8.1" :services ["services/pipeline.puff"]})])])

Resources:
  pkg name ...              apt packages, all a host's installed first, as root
  user name [opts] [setup]  an account (:shell :groups :keys :linger), and
                            resources run as it, ~ as its home
  dir path                  a folder (:mode :owner)
  file path content         a file (:mode :owner :valid), or with
                            {:from path} a file here's content
  line path text            a line in a file, at the end or in place of
                            those :match, a regexp, finds (:valid)
  repo path url             a git repo cloned, never pulled (:ref)
  go-bin pkg@version        a Go program in ~/go/bin, from go install
  ssh-key                   a key made on the host; plan prints its public half
  env path vars             a 0600 environment file: names to {:env "NAME"}
                            or {:cmd "op read ..."}, read here, never printed
  cmd script {:unless s}    a script run unless :unless exits 0 (:name)

Each takes :as, a user to run as (except pkg and user), and :restarts, a
systemd unit restarted once at the end if it changed: the :as user's own.
:valid is a command with %s, as "sshd -t -f %s", that must pass on the new
file before it replaces the old one. :absent #t, on any but cmd, removes
it instead: a file, dir, repo or env by its path alone, a user with its
home once what it runs has ended; what the file doesn't name stays.
Local paths are from the host file's folder. plan exits 0 if each host
matches, 2 if one differs, 1 on an error. The file runs :read, so it
can't change anything itself. whiff comes from whiff.puff in waft's lib
folder (cmd/waft/lib), copied next to the host file: see its comments.
The guide is docs/waft.md in whiff's repository.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, dial))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, dial func(string) (runner, func(), error)) int {
	if len(args) != 2 || args[0] != "plan" && args[0] != "apply" {
		io.WriteString(stderr, usage)
		return 1
	}
	hosts, err := load(ctx, args[1])
	if err != nil {
		fmt.Fprintf(stderr, "waft: %v\n", err)
		return 1
	}
	apply, differs := args[0] == "apply", false
	for _, h := range hosts {
		r, closeRunner, err := dial(h.dest)
		if err == nil {
			var changed bool
			changed, err = converge(ctx, stdout, r, h, apply)
			differs = differs || changed
			closeRunner()
		}
		if err != nil {
			fmt.Fprintf(stderr, "waft: %s: %v\n", h.dest, err)
			return 1
		}
	}
	if differs && !apply {
		return 2
	}
	return 0
}

// load evaluates file, read-only, and returns the hosts it ends with.
func load(ctx context.Context, file string) ([]host, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	env := puff.NewEnv()
	if err := env.Register(builtins(filepath.Dir(file))...); err != nil {
		return nil, err
	}
	v, err := env.LoadFile(puff.WithMark(ctx, "a waft file", puff.MarkRead), file, string(src))
	if err != nil {
		return nil, err
	}
	vs := []puff.Value{v}
	if vec, ok := v.(puff.Vector); ok {
		vs = vs[:0]
		for i := range vec.Len() {
			vs = append(vs, vec.At(i))
		}
	}
	var hosts []host
	for _, v := range vs {
		if v == nil {
			break
		}
		h, err := parseHost(filepath.Dir(file), v)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s: want its last form to be a host, or a vector of hosts", file)
	}
	return hosts, nil
}

// converge checks each of h's resources and prints what differs, with the
// commands that would fix it, and returns whether anything did. With apply, it runs them and checks again,
// stopping at the first resource still off; units that changed resources
// name are restarted once, at the end. In a plan, a check that fails after
// an earlier change is reported, not fatal: it may need that change, as a
// dir :as a user that isn't made yet.
func converge(ctx context.Context, w io.Writer, r runner, h host, apply bool) (changed bool, err error) {
	fmt.Fprintln(w, h.dest)
	var restarts []restart
	made := map[string]bool{} // users this plan makes
	for _, res := range h.resources {
		o := res.opts()
		name := res.String()
		if o.as != "" {
			name += " (as " + o.as + ")"
		}
		if o.absent {
			name += " (absent)"
		}
		fix, err := res.check(ctx, r)
		if err != nil && (apply || !changed) {
			return changed, fmt.Errorf("%s: %v", name, err)
		}
		if err != nil && made[o.as] {
			fmt.Fprintf(w, "  new     %s\n", name)
			restarts = addRestarts(restarts, o)
			continue
		}
		if err != nil {
			fmt.Fprintf(w, "  ?       %s: can't check before the changes above: %v\n", name, err)
			continue
		}
		if len(fix) == 0 {
			fmt.Fprintf(w, "  ok      %s\n", name)
			note(w, res)
			continue
		}
		changed = true
		if u, ok := res.(*user); ok && strings.HasPrefix(fix[0], "useradd") {
			made[u.name] = true
		}
		fmt.Fprintf(w, "  change  %s\n", name)
		for _, c := range fix {
			fmt.Fprintf(w, "            %s\n", short(c))
		}
		restarts = addRestarts(restarts, o)
		if !apply {
			continue
		}
		var input []byte
		if i, ok := res.(inputter); ok {
			input = i.input()
		}
		if err := script(ctx, r, o.as, fix, input); err != nil {
			return changed, fmt.Errorf("%s: %v", name, err)
		}
		if fix, err = res.check(ctx, r); err == nil && len(fix) > 0 {
			err = fmt.Errorf("still differs after apply: %s", short(fix[0]))
		}
		if err != nil {
			return changed, fmt.Errorf("%s: %v", name, err)
		}
		note(w, res)
	}
	if len(restarts) == 0 {
		return changed, nil
	}
	names := make([]string, len(restarts))
	for i, u := range restarts {
		names[i] = u.String()
	}
	fmt.Fprintf(w, "  restart %s\n", strings.Join(names, ", "))
	if !apply {
		return changed, nil
	}
	// System units together, then each user's.
	var users []string
	units := map[string][]string{}
	for _, u := range restarts {
		if _, ok := units[u.user]; !ok {
			users = append(users, u.user)
		}
		units[u.user] = append(units[u.user], u.unit)
	}
	slices.Sort(users)
	for _, user := range users {
		cmds := []string{"systemctl daemon-reload", "systemctl restart " + quoteAll(units[user])}
		if user != "" {
			cmds = []string{userBus, "systemctl --user daemon-reload", "systemctl --user restart " + quoteAll(units[user])}
		}
		if err := script(ctx, r, user, cmds, nil); err != nil {
			return changed, fmt.Errorf("restart: %v", err)
		}
	}
	return changed, nil
}

// addRestarts is restarts with the units o names, each once.
func addRestarts(restarts []restart, o *common) []restart {
	for _, unit := range o.restarts {
		u := restart{o.as, unit}
		if u.user == "root" {
			u.user = ""
		}
		if !slices.Contains(restarts, u) {
			restarts = append(restarts, u)
		}
	}
	return restarts
}

// A restart is a systemd unit restarted at the end: a user's if user is set,
// as a resource run as that user names.
type restart struct{ user, unit string }

func (u restart) String() string {
	if u.user == "" {
		return u.unit
	}
	return u.unit + " (as " + u.user + ")"
}

// userBus lets systemctl --user reach the user's manager under sudo.
const userBus = "export XDG_RUNTIME_DIR=/run/user/$(id -u)"

// An inputter is a resource whose fix reads its stdin: a secret, or a
// binary too big for a script's words.
type inputter interface{ input() []byte }

// A noter is a resource with something to say once it matches, as an ssh
// key's public half.
type noter interface{ note() string }

func note(w io.Writer, res resource) {
	if n, ok := res.(noter); ok && n.note() != "" {
		fmt.Fprintf(w, "            %s\n", n.note())
	}
}

// script runs cmds as one script that stops at the first failing, with
// input on its stdin.
func script(ctx context.Context, r runner, as string, cmds []string, input []byte) error {
	res, err := r.run(ctx, as, "set -e\n"+strings.Join(cmds, "\n"), input)
	if err == nil && res.code != 0 {
		err = fmt.Errorf("exit %d: %s", res.code, res.stderr)
	}
	return err
}

// short is cmd's first line, cut to fit plan's output; a write is what it
// writes, not its bytes.
func short(cmd string) string {
	if what, ok := shownWrite(cmd); ok {
		return what
	}
	line, _, more := strings.Cut(cmd, "\n")
	if len(line) > 100 {
		line, more = line[:97], true
	}
	if more {
		line += "..."
	}
	return line
}
