package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// A resource is part of a host as a waft file describes it.
type resource interface {
	// String names it in plan's output: "pkg git curl".
	String() string
	// check returns the commands that would make the host match it, none
	// if it does. It only reads.
	check(ctx context.Context, r runner) ([]string, error)
	opts() *common
}

// common is what every resource may have.
type common struct {
	as       string   // the user it runs as; "" for root
	restarts []string // units to restart once at the end if it changed: the user's, with as
}

func (c *common) opts() *common { return c }

// kind is a resource's builtin: (pkg "git" "curl") is the tag it returns,
// and parse makes the resource from the tag's values, with local paths
// relative to base. as is whether it takes :as.
type kind struct {
	name, args, doc string
	as              bool
	parse           func(base string, args []puff.Value) (resource, error)
}

var kinds = []kind{
	{"pkg", "name ... [opts]", "apt packages installed, wherever it is: a host's are installed together, first, as root.", false, parsePkg},
	{"user", "name [opts] [setup]", "An account with its home, and its setup: resources run as it, ~ as its home. Options: :shell, :groups (added, never removed), :keys (its whole authorized_keys: keys, or paths of .pub files here), :linger #t (its systemd user units run without a login).", false, parseUser},
	{"dir", "path [opts]", "A folder. Options: :as, :mode such as \"755\", :owner.", true, parseDir},
	{"file", "path content [opts]", "A file's content, or with :from that of a file here, without content. Options: :as, :from, :mode, :owner, :valid (a command with %s, the new file's path, that must pass before it replaces the old).", true, parseFile},
	{"line", "path text [opts]", "A line in a file: added at the end, or with :match, a regexp, in place of each line it finds. Options: :as, :match, :valid as file's.", true, parseLine},
	{"repo", "path url [opts]", "A git repo cloned at path, never pulled. Options: :as, :ref, a branch, tag or commit checked out.", true, parseRepo},
	{"go-bin", "pkg@version [opts]", "A Go program in ~/go/bin, built with go install pkg@version; a version that isn't one (latest, a branch) is installed once, never upgraded. Options: :as.", true, parseGoBin},
	{"ssh-key", "[opts]", "An ed25519 key at ~/.ssh/id_ed25519, made on the host; plan prints its public half. Options: :as.", true, parseKey},
	{"env", "path vars [opts]", "A 0600 environment file of vars, names to {:env \"NAME\"} or {:cmd \"...\"}: each read here, sent on stdin, never printed. Options: :as.", true, parseEnv},
	{"cmd", "script opts", "A script run unless :unless, a script too, exits 0. Options: :as, :name (in plan, for the script's first line).", true, parseCmd},
}

// builtins are waft's functions: host and one per kind. Each checks its
// arguments as the kind will read them, so a mistake faults at its place;
// base is the file's folder.
func builtins(base string) []*puff.Builtin {
	bs := []*puff.Builtin{{Name: "host", Args: "dest setup",
		Doc: "A host: dest as ssh takes it (an admin with passwordless sudo), and its setup, a vector of resources; nested vectors are joined.",
		Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
			t := puff.NewTag("host", args...)
			_, err := parseHost(base, t)
			return t, err
		}}}
	for _, k := range kinds {
		bs = append(bs, &puff.Builtin{Name: k.name, Args: k.args, Doc: k.doc + " Every resource takes :restarts, a systemd unit.",
			Fn: func(_ *puff.Call, args []puff.Value) (puff.Value, error) {
				_, err := k.parse(base, args)
				return puff.NewTag(k.name, args...), err
			}})
	}
	return bs
}

type host struct {
	dest      string
	resources []resource
}

func parseHost(base string, v puff.Value) (host, error) {
	t, ok := v.(puff.Tag)
	if !ok || t.Name() != "host" || t.Len() != 2 {
		return host{}, fmt.Errorf("want a host, got %s", puff.Print(v))
	}
	dest, err := str(t.At(0), "host's dest")
	h := host{dest: dest}
	if err != nil {
		return h, err
	}
	if _, ok := t.At(1).(puff.Vector); !ok {
		return h, fmt.Errorf("want host's setup as a vector of resources, got %s", puff.Print(t.At(1)))
	}
	rs, err := parseResources(base, t.At(1), "")
	// Packages go first, together.
	var p *pkg
	for _, r := range rs {
		if _, ok := r.(*pkg); ok && p == nil {
			p = &pkg{}
			h.resources = append(h.resources, p)
		}
		if q, ok := r.(*pkg); ok {
			p.names = append(p.names, slices.DeleteFunc(slices.Clone(q.names), func(n string) bool { return slices.Contains(p.names, n) })...)
			p.restarts = append(p.restarts, q.restarts...)
		}
	}
	for _, r := range rs {
		if _, ok := r.(*pkg); !ok {
			h.resources = append(h.resources, r)
		}
	}
	return h, err
}

// parseResources reads v, a resource or a vector of them, nested vectors
// joined; those that take :as and don't say it run as as.
func parseResources(base string, v puff.Value, as string) ([]resource, error) {
	if vec, ok := v.(puff.Vector); ok {
		var rs []resource
		for i := range vec.Len() {
			more, err := parseResources(base, vec.At(i), as)
			if err != nil {
				return nil, err
			}
			rs = append(rs, more...)
		}
		return rs, nil
	}
	t, ok := v.(puff.Tag)
	i := slices.IndexFunc(kinds, func(k kind) bool { return ok && k.name == t.Name() })
	if i < 0 {
		return nil, fmt.Errorf("want a resource, got %s", puff.Print(v))
	}
	args := make([]puff.Value, t.Len())
	for j := range args {
		args[j] = t.At(j)
	}
	res, err := kinds[i].parse(base, args)
	if err != nil {
		return nil, err
	}
	if o := res.opts(); kinds[i].as && o.as == "" {
		o.as = as
	}
	u, ok := res.(*user)
	if ok && as != "" {
		return nil, fmt.Errorf("a user's setup can't hold another user, as %s", u.name)
	}
	if !ok || u.setup == nil {
		return []resource{res}, nil
	}
	setup, err := parseResources(base, u.setup, u.name)
	if err != nil {
		return nil, fmt.Errorf("user %s: %v", u.name, err)
	}
	return append([]resource{res}, setup...), nil
}

type pkg struct {
	common
	names []string
}

func parsePkg(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args)
	p := &pkg{}
	if err == nil {
		p.names, err = strs(args, "pkg's names")
	}
	if err == nil && len(p.names) == 0 {
		err = fmt.Errorf("pkg takes package names")
	}
	return p, first(err, p.common.set(o))
}

func (p *pkg) String() string { return "pkg " + strings.Join(p.names, " ") }

func (p *pkg) check(ctx context.Context, r runner) ([]string, error) {
	out, err := probe(ctx, r, "", `for p in `+quoteAll(p.names)+`; do
dpkg-query -W -f='${db:Status-Status}\n' "$p" 2>/dev/null | grep -qx installed || echo "$p"
done`)
	if missing := strings.Fields(out); len(missing) > 0 {
		return []string{"apt-get update -q", "DEBIAN_FRONTEND=noninteractive apt-get install -y -q " + quoteAll(missing)}, err
	}
	return nil, err
}

type user struct {
	common
	name, shell        string
	groups, keys       []string
	manageKeys, linger bool
	setup              puff.Value // resources run as it, if any
}

func parseUser(base string, args []puff.Value) (resource, error) {
	u := &user{}
	if n := len(args); n > 1 {
		if _, ok := args[n-1].(puff.Vector); ok {
			u.setup, args = args[n-1], args[:n-1]
		}
	}
	args, o, err := splitOpts(args, "shell", "groups", "keys", "linger")
	if err == nil && len(args) != 1 {
		err = fmt.Errorf("user takes a name, options and a setup")
	}
	if err == nil {
		u.name, err = str(args[0], "user's name")
	}
	_, u.manageKeys = o["keys"]
	err = first(err, optStr(o, "shell", &u.shell), optStrs(o, "groups", &u.groups),
		optStrs(o, "keys", &u.keys), optBool(o, "linger", &u.linger), u.common.set(o))
	u.keys = localKeys(base, u.keys)
	return u, err
}

func (u *user) String() string { return "user " + u.name }

func (u *user) check(ctx context.Context, r runner) ([]string, error) {
	n := quote(u.name)
	out, err := probe(ctx, r, "", `getent passwd `+n+` | cut -d: -f7
id -nG `+n+` 2>/dev/null || echo
[ -f /var/lib/systemd/linger/`+n+` ] && echo linger || echo
home=$(getent passwd `+n+` | cut -d: -f6)
[ -z "$home" ] || cat "$home/.ssh/authorized_keys" 2>/dev/null || true`)
	if err != nil {
		return nil, err
	}
	got := strings.SplitN(out, "\n", 4)
	for len(got) < 4 {
		got = append(got, "")
	}
	var fix []string
	if got[0] == "" {
		shell := u.shell
		if shell == "" {
			shell = "/bin/bash"
		}
		cmd := "useradd -m -s " + quote(shell)
		if len(u.groups) > 0 {
			cmd += " -G " + quote(strings.Join(u.groups, ","))
		}
		fix = append(fix, cmd+" "+n)
		got = []string{"", "", "", ""}
	} else {
		if u.shell != "" && got[0] != u.shell {
			fix = append(fix, "usermod -s "+quote(u.shell)+" "+n)
		}
		have := strings.Fields(got[1])
		if missing := slices.DeleteFunc(slices.Clone(u.groups), func(g string) bool { return slices.Contains(have, g) }); len(missing) > 0 {
			fix = append(fix, "usermod -aG "+quote(strings.Join(missing, ","))+" "+n)
		}
	}
	if u.linger && got[2] != "linger" {
		fix = append(fix, "loginctl enable-linger "+n)
	}
	if !u.manageKeys {
		return fix, nil
	}
	want, err := readKeys(u.keys)
	if err != nil || got[3] == strings.TrimSuffix(want, "\n") {
		return fix, err
	}
	keys := `"$home/.ssh/authorized_keys"`
	return append(fix, "home=$(getent passwd "+n+" | cut -d: -f6)",
		`install -d -m 700 "$home/.ssh"`,
		writeCmd(want, keys), "chmod 600 "+keys,
		"chown -R "+n+": "+`"$home/.ssh"`), nil
}

// localKeys is keys with each path of a .pub file here made a path from
// base; a key stays as it is.
func localKeys(base string, keys []string) []string {
	ks := slices.Clone(keys)
	for i, k := range ks {
		if isKeyPath(k) {
			ks[i] = local(base, k)
		}
	}
	return ks
}

func isKeyPath(k string) bool {
	return strings.HasPrefix(k, "~/") || strings.HasPrefix(k, "/") || strings.HasPrefix(k, ".")
}

// local is the path here that p, a path in a waft file, names: from the
// home folder with ~/, else from base, the file's folder.
func local(base, p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) || base == "" {
		return p
	}
	return filepath.Join(base, p)
}

// readKeys is the authorized_keys holding keys: each a key, or the path of a
// .pub file here.
func readKeys(keys []string) (string, error) {
	var b strings.Builder
	for _, k := range keys {
		if isKeyPath(k) {
			data, err := os.ReadFile(local("", k))
			if err != nil {
				return "", err
			}
			k = string(data)
		}
		b.WriteString(strings.TrimSpace(k))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

type dir struct {
	common
	path, mode, owner string
}

func parseDir(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as", "mode", "owner")
	d := &dir{}
	if err == nil && len(args) != 1 {
		err = fmt.Errorf("dir takes a path and options")
	}
	if err == nil {
		d.path, err = str(args[0], "dir's path")
	}
	return d, first(err, optMode(o, &d.mode), optStr(o, "owner", &d.owner), d.common.set(o))
}

func (d *dir) String() string { return "dir " + d.path }

func (d *dir) check(ctx context.Context, r runner) ([]string, error) {
	p := shPath(d.path)
	out, err := probe(ctx, r, d.as, `[ ! -d `+p+` ] || stat -c '%U %a' `+p)
	var fix []string
	if out == "" {
		fix = []string{"mkdir -p " + p}
	}
	return append(fix, attrs(p, out, d.mode, d.owner)...), err
}

// file is a file's content, or, with from, that of a file here, read as
// it's checked.
type file struct {
	common
	path, content, from, mode, owner, valid string
}

func parseFile(base string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as", "from", "mode", "owner", "valid")
	f := &file{}
	err = first(err, optStr(o, "from", &f.from), optMode(o, &f.mode), optStr(o, "owner", &f.owner),
		optValid(o, &f.valid), f.common.set(o))
	if err == nil && len(args) != 2 && (f.from == "" || len(args) != 1) {
		err = fmt.Errorf("file takes a path, its content and options, or a path and {:from path}")
	}
	if err == nil {
		f.path, err = str(args[0], "file's path")
	}
	if err == nil && f.from == "" {
		// Content may be empty, as an empty file.
		c, ok := args[1].(puff.String)
		if !ok {
			err = fmt.Errorf("file's content: want a string, got %s", puff.Print(args[1]))
		}
		f.content = string(c)
	}
	if f.from != "" {
		f.from = local(base, f.from)
	}
	return f, err
}

func (f *file) String() string { return "file " + f.path }

func (f *file) check(ctx context.Context, r runner) ([]string, error) {
	if f.from != "" {
		data, err := os.ReadFile(f.from)
		if err != nil {
			return nil, err
		}
		f.content = string(data)
	}
	p := shPath(f.path)
	out, err := probe(ctx, r, f.as, `[ ! -f `+p+` ] || { sha256sum < `+p+` | cut -d' ' -f1; stat -c '%U %a' `+p+`; }`)
	hash, attr, _ := strings.Cut(out, "\n")
	sum := sha256.Sum256([]byte(f.content))
	var fix []string
	if hash == "" {
		fix = []string{`mkdir -p "$(dirname -- ` + p + `)"`}
	}
	if hash != hex.EncodeToString(sum[:]) {
		fix = append(fix, writeValid(f.content, p, f.valid)...)
	}
	return append(fix, attrs(p, attr, f.mode, f.owner)...), err
}

// line is a line in a file: there, or, with match, in place of each line
// match finds.
type line struct {
	common
	path, text, valid string
	match             *regexp.Regexp
}

func parseLine(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as", "match", "valid")
	l := &line{}
	if err == nil && len(args) != 2 {
		err = fmt.Errorf("line takes a path, the line and options")
	}
	if err == nil {
		l.path, err = str(args[0], "line's path")
	}
	if err == nil {
		l.text, err = str(args[1], "line's text")
	}
	if err == nil && strings.Contains(l.text, "\n") {
		err = fmt.Errorf("line: %q is more than a line", l.text)
	}
	var match string
	err = first(err, optStr(o, "match", &match), optValid(o, &l.valid), l.common.set(o))
	if err == nil && match != "" {
		if l.match, err = regexp.Compile(match); err != nil {
			err = fmt.Errorf("line's :match: %v", err)
		}
	}
	return l, err
}

func (l *line) String() string { return "line " + l.path + ": " + l.text }

func (l *line) check(ctx context.Context, r runner) ([]string, error) {
	p := shPath(l.path)
	// A mark first, so an empty file isn't a missing one.
	out, err := probe(ctx, r, l.as, `[ ! -f `+p+` ] || { echo .; cat `+p+`; }`)
	if err != nil {
		return nil, err
	}
	var old string
	if rest, ok := strings.CutPrefix(out, ".\n"); ok {
		old = rest + "\n" // probe took the last newline
	}
	want := withLine(old, l.text, l.match)
	if want == old {
		return nil, nil
	}
	var fix []string
	if out == "" {
		fix = []string{`mkdir -p "$(dirname -- ` + p + `)"`}
	}
	return append(fix, writeValid(want, p, l.valid)...), nil
}

// withLine is content with text as a line: each line match finds becomes
// text; if none does, and text isn't there, it's added at the end.
func withLine(content, text string, match *regexp.Regexp) string {
	var lines []string
	if content != "" {
		lines = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	replaced := false
	for i, l := range lines {
		if match != nil && match.MatchString(l) {
			lines[i], replaced = text, true
		}
	}
	if !replaced && !slices.Contains(lines, text) {
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n") + "\n"
}

type cmd struct {
	common
	script, unless string
	name           string // its name in plan, if not its script's first line
}

func parseCmd(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as", "unless", "name")
	c := &cmd{}
	if err == nil && len(args) != 1 {
		err = fmt.Errorf("cmd takes a script and options")
	}
	if err == nil {
		c.script, err = str(args[0], "cmd's script")
	}
	if err == nil && o["unless"] == nil {
		err = fmt.Errorf("cmd needs :unless, a script that exits 0 once it has run")
	}
	return c, first(err, optStr(o, "unless", &c.unless), optStr(o, "name", &c.name), c.common.set(o))
}

func (c *cmd) String() string {
	if c.name != "" {
		return "cmd " + c.name
	}
	line, _, _ := strings.Cut(c.script, "\n")
	return "cmd " + line
}

// check runs :unless and reports its exit, so a failing sudo, as for a user
// not made yet, is an error, not a change.
func (c *cmd) check(ctx context.Context, r runner) ([]string, error) {
	out, err := probe(ctx, r, c.as, "("+c.unless+") >/dev/null 2>&1 </dev/null; echo $?")
	if err != nil || out == "0" {
		return nil, err
	}
	return []string{c.script}, nil
}

type repo struct {
	common
	path, url, ref string
}

func parseRepo(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as", "ref")
	g := &repo{}
	if err == nil && len(args) != 2 {
		err = fmt.Errorf("repo takes a path, a git URL and options")
	}
	if err == nil {
		g.path, err = str(args[0], "repo's path")
	}
	if err == nil {
		g.url, err = str(args[1], "repo's URL")
	}
	return g, first(err, optStr(o, "ref", &g.ref), g.common.set(o))
}

func (g *repo) String() string { return "repo " + g.path }

// gitSSH has git's first ssh to a host trust its key, as a clone from a
// fresh host must.
const gitSSH = "GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new' "

func (g *repo) check(ctx context.Context, r runner) ([]string, error) {
	p := shPath(g.path)
	out, err := probe(ctx, r, g.as, `[ -d `+p+`/.git ] || exit 0
git -C `+p+` remote get-url origin
git -C `+p+` symbolic-ref -q --short HEAD || git -C `+p+` rev-parse HEAD`)
	if err != nil {
		return nil, err
	}
	if out == "" {
		clone := gitSSH + "git clone -q "
		if g.ref != "" {
			clone += "-b " + quote(g.ref) + " "
		}
		return []string{clone + quote(g.url) + " " + p}, nil
	}
	origin, head, _ := strings.Cut(out, "\n")
	var fix []string
	if origin != g.url {
		fix = append(fix, "git -C "+p+" remote set-url origin "+quote(g.url))
	}
	if g.ref != "" && head != g.ref && !strings.HasPrefix(head, g.ref) {
		fix = append(fix, gitSSH+"git -C "+p+" fetch -q origin", "git -C "+p+" checkout -q "+quote(g.ref))
	}
	return fix, nil
}

// probe runs a script that only reads, and returns its output without the
// trailing newline; a nonzero exit is an error.
func probe(ctx context.Context, r runner, as, script string) (string, error) {
	res, err := r.run(ctx, as, script, nil)
	if err == nil && res.code != 0 {
		err = fmt.Errorf("exit %d: %s", res.code, res.stderr)
	}
	return strings.TrimSuffix(res.out, "\n"), err
}

// writeCmd writes content to p, a path for sh; an existing file keeps its
// owner and mode.
func writeCmd(content, p string) string {
	return "printf %s " + quote(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + p
}

// writeValid writes content to p, and with valid, a command with %s for a
// path, writes it beside p first and moves it in place only if valid
// passes on it; the copy keeps p's owner and mode.
func writeValid(content, p, valid string) []string {
	if valid == "" {
		return []string{writeCmd(content, p)}
	}
	t := p + ".waft"
	return []string{"[ ! -f " + p + " ] || cp -p " + p + " " + t,
		writeCmd(content, t),
		strings.ReplaceAll(valid, "%s", t) + " || { rm -f " + t + "; exit 1; }",
		"mv " + t + " " + p}
}

func optValid(o map[string]puff.Value, valid *string) error {
	err := optStr(o, "valid", valid)
	if err == nil && *valid != "" && !strings.Contains(*valid, "%s") {
		err = fmt.Errorf(":valid: want %%s in it, for the new file's path, as \"sshd -t -f %%s\"")
	}
	return err
}

// shownWrite is how plan shows cmd if writeCmd made it: the path and how
// much it writes.
func shownWrite(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, "printf %s '")
	b64, p, ok2 := strings.Cut(rest, "' | base64 -d > ")
	data, err := base64.StdEncoding.DecodeString(b64)
	if !ok || !ok2 || err != nil {
		return "", false
	}
	p = strings.NewReplacer(`"$HOME"/`, "~/", `'`, "", `"`, "").Replace(p)
	unit := "bytes"
	if len(data) == 1 {
		unit = "byte"
	}
	return fmt.Sprintf("write %s (%d %s)", p, len(data), unit), true
}

// attrs are the commands that give p, now with stat's "owner mode" (empty if
// it doesn't exist yet), the mode and owner wanted, where those are set.
func attrs(p, stat, mode, owner string) []string {
	gotOwner, gotMode, _ := strings.Cut(stat, " ")
	var fix []string
	if mode != "" && strings.TrimLeft(gotMode, "0") != mode {
		fix = append(fix, "chmod "+mode+" "+p)
	}
	if owner != "" && gotOwner != owner {
		fix = append(fix, "chown "+quote(owner)+": "+p)
	}
	return fix
}

// splitOpts splits a trailing options map off args, failing on a key that
// isn't allowed or common to every resource.
func splitOpts(args []puff.Value, allowed ...string) ([]puff.Value, map[string]puff.Value, error) {
	n := len(args)
	m, ok := puff.Map{}, false
	if n > 0 {
		m, ok = args[n-1].(puff.Map)
	}
	if !ok {
		return args, map[string]puff.Value{}, nil
	}
	o, err := fields(m, "options", append(allowed, "restarts")...)
	return args[:n-1], o, err
}

// fields is m's entries by keyword, failing on a key not allowed.
func fields(m puff.Map, what string, allowed ...string) (map[string]puff.Value, error) {
	o := map[string]puff.Value{}
	for i := range m.Len() {
		e := m.Entry(i)
		k, ok := e.Key.(puff.Keyword)
		if !ok || !slices.Contains(allowed, string(k)) {
			return nil, fmt.Errorf("%s: unknown key %s: want :%s", what, puff.Print(e.Key), strings.Join(allowed, " :"))
		}
		o[string(k)] = e.Val
	}
	return o, nil
}

func (c *common) set(o map[string]puff.Value) error {
	var unit string
	err := first(optStr(o, "as", &c.as), optStr(o, "restarts", &unit))
	if unit != "" {
		c.restarts = append(c.restarts, unit)
	}
	return err
}

func optStr(o map[string]puff.Value, key string, s *string) (err error) {
	if v, ok := o[key]; ok {
		*s, err = str(v, ":"+key)
	}
	return err
}

func optStrs(o map[string]puff.Value, key string, ss *[]string) error {
	v, ok := o[key]
	if !ok {
		return nil
	}
	vec, ok := v.(puff.Vector)
	if !ok {
		return fmt.Errorf(":%s: want a vector of strings, got %s", key, puff.Print(v))
	}
	args := make([]puff.Value, vec.Len())
	for i := range args {
		args[i] = vec.At(i)
	}
	var err error
	*ss, err = strs(args, ":"+key)
	return err
}

func optMode(o map[string]puff.Value, mode *string) error {
	if err := optStr(o, "mode", mode); err != nil || *mode == "" {
		return err
	}
	if len(*mode) > 4 || strings.Trim(*mode, "01234567") != "" {
		return fmt.Errorf(":mode: want octal digits such as \"755\", got %q", *mode)
	}
	*mode = strings.TrimLeft(*mode, "0")
	return nil
}

func str(v puff.Value, what string) (string, error) {
	s, ok := v.(puff.String)
	if !ok || s == "" {
		return "", fmt.Errorf("%s: want a string, got %s", what, puff.Print(v))
	}
	return string(s), nil
}

func strs(vs []puff.Value, what string) ([]string, error) {
	ss := make([]string, len(vs))
	for i, v := range vs {
		var err error
		if ss[i], err = str(v, what); err != nil {
			return nil, err
		}
	}
	return ss, nil
}

func first(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
