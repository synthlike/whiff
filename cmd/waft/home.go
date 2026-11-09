package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/synthlike/whiff/internal/puff"
)

// key is an ed25519 key made for a user on the host, as git's; its private
// half never leaves it.
type key struct {
	common
	pub string // the public half, once checked
}

func parseKey(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as")
	k := &key{}
	if err == nil && len(args) != 0 {
		err = fmt.Errorf("ssh-key takes only options")
	}
	return k, first(err, k.common.set(o))
}

func (k *key) String() string { return "ssh-key ~/.ssh/id_ed25519" }

func (k *key) note() string { return k.pub }

func (k *key) check(ctx context.Context, r runner) ([]string, error) {
	if k.absent {
		return k.gone(ctx, r)
	}
	out, err := probe(ctx, r, k.as, `[ ! -f "$HOME/.ssh/id_ed25519" ] || cat "$HOME/.ssh/id_ed25519.pub"`)
	if err != nil || out != "" {
		k.pub = out
		return nil, err
	}
	return []string{`install -d -m 700 "$HOME/.ssh"`,
		`ssh-keygen -q -t ed25519 -N '' -C "$(id -un)@$(hostname)" -f "$HOME/.ssh/id_ed25519"`}, nil
}

// env is a 0600 environment file of secrets, each read here, from an
// environment variable or a command's output, as it's checked.
type env struct {
	common
	path    string
	vars    []envVar
	content []byte // once read
}

type envVar struct{ name, from, cmd string }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var commit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func parseEnv(_ string, args []puff.Value) (resource, error) {
	// The vars are a map too, with strings for keys: options, with
	// keywords, follow them, or follow the path alone.
	o, err := map[string]puff.Value{}, error(nil)
	if n := len(args); n == 3 || n == 2 && hasKeyword(args[1]) {
		args, o, err = splitOpts(args, "as")
	}
	e := &env{}
	err = first(err, e.common.set(o))
	if err == nil && len(args) != 2 && (!e.absent || len(args) != 1) {
		err = fmt.Errorf("env takes a path, its vars and options")
	}
	if err == nil {
		e.path, err = str(args[0], "env's path")
	}
	if err == nil && len(args) == 2 {
		err = entries(args[1], "env's vars", func(name string, v puff.Value) error {
			ev, err := parseEnvVar(name, v)
			e.vars = append(e.vars, ev)
			return err
		})
	}
	return e, err
}

// hasKeyword is whether v is a map with a keyword key: options, not vars.
func hasKeyword(v puff.Value) bool {
	m, ok := v.(puff.Map)
	if ok && m.Len() > 0 {
		_, ok = m.Entry(m.Len() - 1).Key.(puff.Keyword)
		return ok
	}
	return false
}

func parseEnvVar(name string, v puff.Value) (envVar, error) {
	e := envVar{name: name}
	if !envName.MatchString(name) {
		return e, fmt.Errorf("env: %q isn't a variable name", name)
	}
	m, ok := v.(puff.Map)
	if !ok {
		return e, fmt.Errorf("env %s: want {:env \"NAME\"} or {:cmd \"...\"}, got %s: a value never goes in the file", name, puff.Print(v))
	}
	o, err := fields(m, "env "+name, "env", "cmd")
	if err == nil {
		err = first(optStr(o, "env", &e.from), optStr(o, "cmd", &e.cmd))
	}
	if err == nil && (e.from == "") == (e.cmd == "") {
		err = fmt.Errorf("env %s: want one of :env and :cmd", name)
	}
	return e, err
}

func (e *env) String() string { return "env " + e.path }

func (e *env) input() []byte { return e.content }

func (e *env) check(ctx context.Context, r runner) ([]string, error) {
	if e.absent {
		return e.gone(ctx, r)
	}
	if err := e.read(ctx); err != nil {
		return nil, err
	}
	p := shPath(e.path)
	out, err := probe(ctx, r, e.as, `[ ! -f `+p+` ] || { sha256sum < `+p+` | cut -d' ' -f1; stat -c '%a' `+p+`; }`)
	if err != nil {
		return nil, err
	}
	hash, mode, _ := strings.Cut(out, "\n")
	sum := sha256.Sum256(e.content)
	if hash == hex.EncodeToString(sum[:]) && mode == "600" {
		return nil, nil
	}
	names := make([]string, len(e.vars))
	for i, v := range e.vars {
		names[i] = v.name
	}
	// Plan prints these lines: the values come on stdin, never in them.
	return []string{"# write " + e.path + ": " + strings.Join(names, " "),
		`mkdir -p "$(dirname -- ` + p + `)"`,
		"umask 077; cat > " + p + ".new",
		"chmod 600 " + p + ".new",
		"mv " + p + ".new " + p}, nil
}

// read reads each variable's value here, once, into the file's content.
func (e *env) read(ctx context.Context) error {
	if e.content != nil {
		return nil
	}
	var b strings.Builder
	for _, v := range e.vars {
		val := os.Getenv(v.from)
		if v.from != "" && val == "" {
			return fmt.Errorf("%s: $%s isn't set here", v.name, v.from)
		}
		if v.cmd != "" {
			c := exec.CommandContext(ctx, "sh", "-c", v.cmd)
			var stderr bytes.Buffer
			c.Stderr = &stderr
			out, err := c.Output()
			if err != nil {
				return fmt.Errorf("%s: %s: %v: %s", v.name, v.cmd, err, bytes.TrimSpace(stderr.Bytes()))
			}
			val = strings.TrimSuffix(string(out), "\n")
		}
		if strings.ContainsAny(val, "\r\n") {
			return fmt.Errorf("%s: its value has a newline", v.name)
		}
		// systemd's EnvironmentFile reads a double-quoted value with
		// backslash escapes.
		val = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(val)
		fmt.Fprintf(&b, "%s=\"%s\"\n", v.name, val)
	}
	e.content = []byte(b.String())
	return nil
}

// goBin is a Go program in ~/go/bin, built with go install.
type goBin struct {
	common
	pkg, version string
}

func parseGoBin(_ string, args []puff.Value) (resource, error) {
	args, o, err := splitOpts(args, "as")
	g := &goBin{}
	if err == nil && len(args) != 1 {
		err = fmt.Errorf("go-bin takes pkg@version and options")
	}
	var s string
	if err == nil {
		s, err = str(args[0], "go-bin's package")
	}
	if err == nil {
		var ok bool
		if g.pkg, g.version, ok = strings.Cut(s, "@"); !ok || g.pkg == "" || g.version == "" {
			err = fmt.Errorf("go-bin: want pkg@version, as \"golang.org/x/tools/gopls@v0.20.0\", got %q", s)
		}
	}
	return g, first(err, g.common.set(o))
}

func (g *goBin) String() string { return "go-bin " + g.pkg + "@" + g.version }

func (g *goBin) check(ctx context.Context, r runner) ([]string, error) {
	if g.absent {
		return g.gone(ctx, r)
	}
	bin := `"$HOME"/go/bin/` + quote(path.Base(g.pkg))
	out, err := probe(ctx, r, g.as, `[ ! -f `+bin+` ] || go version -m `+bin)
	if err != nil {
		return nil, err
	}
	install := []string{"go install " + quote(g.pkg+"@"+g.version)}
	if out == "" {
		return install, nil
	}
	var pkg, version string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "path":
			pkg = f[1]
		case len(f) >= 3 && f[0] == "mod":
			version = f[2]
		}
	}
	if pkg != g.pkg || !sameVersion(version, g.version) {
		return install, nil
	}
	return nil, nil
}

// sameVersion is whether a binary built at got, as go version -m shows it,
// is at want, as go install takes it. A tag must match; a commit matches
// the pseudo-version's; latest or a branch matches anything, as it can't
// be told without the network.
func sameVersion(got, want string) bool {
	switch {
	case strings.HasPrefix(want, "v"):
		return got == want
	case commit.MatchString(want):
		hash := got[strings.LastIndex(got, "-")+1:]
		return len(hash) >= 7 && (strings.HasPrefix(want, hash) || strings.HasPrefix(hash, want))
	}
	return true
}

// entries calls f with each of v's entries, v a map with string keys.
func entries(v puff.Value, what string, f func(k string, v puff.Value) error) error {
	m, ok := v.(puff.Map)
	if !ok {
		return fmt.Errorf("%s: want a map, got %s", what, puff.Print(v))
	}
	for i := range m.Len() {
		e := m.Entry(i)
		k, err := str(e.Key, what+"'s key")
		if err == nil {
			err = f(k, e.Val)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func optBool(o map[string]puff.Value, key string, b *bool) error {
	v, ok := o[key]
	if !ok {
		return nil
	}
	pb, ok := v.(puff.Bool)
	if !ok {
		return fmt.Errorf(":%s: want #t or #f, got %s", key, puff.Print(v))
	}
	*b = bool(pb)
	return nil
}
