package main

import (
	"context"
	"path"
	"strings"
)

// Each resource's gone is its check with :absent: the commands that remove
// it, none once it isn't there.

// exists runs test, a test of p such as -e, as as, and reports it.
func exists(ctx context.Context, r runner, as, test, p string) (bool, error) {
	out, err := probe(ctx, r, as, `[ ! `+test+` `+p+` ] || echo y`)
	return out == "y", err
}

// removeIf is cmds if p passes test, as -e.
func removeIf(ctx context.Context, r runner, as, test, p string, cmds ...string) ([]string, error) {
	there, err := exists(ctx, r, as, test, p)
	if err != nil || !there {
		return nil, err
	}
	return cmds, nil
}

func (f *file) gone(ctx context.Context, r runner) ([]string, error) {
	p := shPath(f.path)
	return removeIf(ctx, r, f.as, "-e", p, "rm -f "+p)
}

func (e *env) gone(ctx context.Context, r runner) ([]string, error) {
	p := shPath(e.path)
	return removeIf(ctx, r, e.as, "-e", p, "rm -f "+p)
}

func (d *dir) gone(ctx context.Context, r runner) ([]string, error) {
	p := shPath(d.path)
	return removeIf(ctx, r, d.as, "-d", p, "rm -r "+p)
}

func (g *repo) gone(ctx context.Context, r runner) ([]string, error) {
	p := shPath(g.path)
	return removeIf(ctx, r, g.as, "-e", p, "rm -rf "+p)
}

func (g *goBin) gone(ctx context.Context, r runner) ([]string, error) {
	bin := `"$HOME"/go/bin/` + quote(path.Base(g.pkg))
	return removeIf(ctx, r, g.as, "-e", bin, "rm -f "+bin)
}

func (k *key) gone(ctx context.Context, r runner) ([]string, error) {
	return removeIf(ctx, r, k.as, "-e", `"$HOME/.ssh/id_ed25519"`, `rm -f "$HOME/.ssh/id_ed25519" "$HOME/.ssh/id_ed25519.pub"`)
}

func (p *pkg) gone(ctx context.Context, r runner) ([]string, error) {
	out, err := probe(ctx, r, "", `for p in `+quoteAll(p.names)+`; do
dpkg-query -W -f='${db:Status-Status}\n' "$p" 2>/dev/null | grep -qx installed && echo "$p"
done; true`)
	if there := strings.Fields(out); len(there) > 0 {
		return []string{"DEBIAN_FRONTEND=noninteractive apt-get remove -y -q " + quoteAll(there)}, err
	}
	return nil, err
}

// gone for a user ends what it runs first: userdel refuses a user with a
// process, and its systemd user manager outlives disabling linger.
func (u *user) gone(ctx context.Context, r runner) ([]string, error) {
	n := quote(u.name)
	out, err := probe(ctx, r, "", `! getent passwd `+n+` >/dev/null || echo y`)
	if err != nil || out != "y" {
		return nil, err
	}
	return []string{"loginctl disable-linger " + n + " 2>/dev/null || true",
		"loginctl terminate-user " + n + " 2>/dev/null || true",
		"i=0; while pgrep -u " + n + " >/dev/null && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done",
		"pkill -KILL -u " + n + " || true",
		// userdel -r warns, and may fail, over a missing mail spool; the
		// user being gone is what counts.
		"userdel -r " + n + " || ! getent passwd " + n + " >/dev/null"}, nil
}

func (l *line) gone(ctx context.Context, r runner) ([]string, error) {
	p := shPath(l.path)
	out, err := probe(ctx, r, l.as, `[ ! -f `+p+` ] || { echo .; cat `+p+`; }`)
	rest, ok := strings.CutPrefix(out, ".\n")
	if err != nil || !ok {
		return nil, err
	}
	old := rest + "\n"
	var kept []string
	for _, s := range strings.Split(rest, "\n") {
		if s != l.text && (l.match == nil || !l.match.MatchString(s)) {
			kept = append(kept, s)
		}
	}
	want := ""
	if len(kept) > 0 {
		want = strings.Join(kept, "\n") + "\n"
	}
	if want == old {
		return nil, nil
	}
	return writeValid(want, p, l.valid), nil
}
