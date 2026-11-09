package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake is a host that answers scripts with results in turn, recording each
// script and the user it ran as.
type fake struct {
	results []result
	ran     []string
	inputs  []string
}

func (f *fake) run(_ context.Context, as, script string, input []byte) (result, error) {
	f.ran = append(f.ran, as+"$ "+script)
	f.inputs = append(f.inputs, string(input))
	if len(f.results) == 0 {
		return result{}, nil
	}
	res := f.results[0]
	f.results = f.results[1:]
	return res, nil
}

func write(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.puff")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	hosts, err := load(context.Background(), write(t, `
(def (role who) [(user who {:groups ["adm"]}) (dir "~/src" {:as who :mode "0755"})])
[(host "admin@a" [(pkg "git") (role "whiff")])
 (host "admin@b" [(cmd "touch /x" {:unless "test -e /x" :restarts "ssh"})])]`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hosts {
		for _, r := range h.resources {
			got = append(got, h.dest+" "+r.String()+" "+r.opts().as+" "+fmt.Sprint(r.opts().restarts))
		}
	}
	want := "admin@a pkg git  []|admin@a user whiff  []|admin@a dir ~/src whiff []|admin@b cmd touch /x  [ssh]"
	if strings.Join(got, "|") != want {
		t.Errorf("got %q\nwant %q", strings.Join(got, "|"), want)
	}
	if d := hosts[0].resources[2].(*dir); d.mode != "755" {
		t.Errorf("mode = %q, want 755", d.mode)
	}

	for _, tc := range []struct{ src, err string }{
		{`(host "a" [(pkg "git" {:as "x"})])`, "options: unknown key :as"},
		{`(host "a" [(cmd "x" {})])`, "cmd needs :unless"},
		{`(host "a" [(dir "/x" {:mode "999"})])`, ":mode: want octal digits"},
		{`(host "a" [1])`, "want a resource, got 1"},
		{`(pkg "git")`, "want a host"},
		{`(def x 1)`, "want its last form to be a host"},
		{`(file/write "/tmp/waft-test" "x") (host "a" [])`, "file/write is :write"},
	} {
		if _, err := load(context.Background(), write(t, tc.src)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("load %s = %v, want %q", tc.src, err, tc.err)
		}
	}
}

func TestChecks(t *testing.T) {
	pub := filepath.Join(t.TempDir(), "id.pub")
	os.WriteFile(pub, []byte("ssh-ed25519 AAAA me\n"), 0o644)
	hash := "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" // of "x"
	for _, tc := range []struct {
		name string
		res  resource
		out  result
		want []string
	}{
		{"pkg ok", &pkg{names: []string{"git"}}, result{}, nil},
		{"pkg missing", &pkg{names: []string{"git", "curl"}}, result{out: "curl\n"},
			[]string{"apt-get update -q", "DEBIAN_FRONTEND=noninteractive apt-get install -y -q 'curl'"}},
		{"new user", &user{name: "whiff", groups: []string{"adm"}}, result{out: "\n"},
			[]string{"useradd -m -s '/bin/bash' -G 'adm' 'whiff'"}},
		{"user ok", &user{name: "whiff", groups: []string{"adm"}, keys: []string{pub}, manageKeys: true},
			result{out: "/bin/bash\nwhiff adm\n\nssh-ed25519 AAAA me\n"}, nil},
		{"user differs", &user{name: "whiff", shell: "/bin/sh", groups: []string{"adm", "sudo"}},
			result{out: "/bin/bash\nwhiff adm\n"}, []string{"usermod -s '/bin/sh' 'whiff'", "usermod -aG 'sudo' 'whiff'"}},
		{"user keys", &user{name: "u", keys: []string{"ssh-ed25519 BBBB"}, manageKeys: true},
			result{out: "/bin/bash\nu\n\nssh-ed25519 AAAA me\n"}, []string{
				"home=$(getent passwd 'u' | cut -d: -f6)", `install -d -m 700 "$home/.ssh"`,
				`printf %s 'c3NoLWVkMjU1MTkgQkJCQgo=' | base64 -d > "$home/.ssh/authorized_keys"`,
				`chmod 600 "$home/.ssh/authorized_keys"`, `chown -R 'u': "$home/.ssh"`}},
		{"linger", &user{name: "u", linger: true}, result{out: "/bin/bash\nu\n\n"}, []string{"loginctl enable-linger 'u'"}},
		{"lingers", &user{name: "u", linger: true}, result{out: "/bin/bash\nu\nlinger\n"}, nil},
		{"new dir", &dir{path: "~/src", mode: "750"}, result{}, []string{`mkdir -p "$HOME"/'src'`, `chmod 750 "$HOME"/'src'`}},
		{"dir ok", &dir{path: "/srv", mode: "755", owner: "whiff"}, result{out: "whiff 755\n"}, nil},
		{"dir owner", &dir{path: "/srv", owner: "whiff"}, result{out: "root 755\n"}, []string{"chown 'whiff': '/srv'"}},
		{"file ok", &file{path: "/f", content: "x"}, result{out: hash + "\nroot 644\n"}, nil},
		{"file mode", &file{path: "/f", content: "x", mode: "600"}, result{out: hash + "\nroot 644\n"}, []string{"chmod 600 '/f'"}},
		{"file differs", &file{path: "/f", content: "x"}, result{out: "abc\nroot 644\n"}, []string{"printf %s 'eA==' | base64 -d > '/f'"}},
		{"file missing", &file{path: "~/.config/x", content: "x"}, result{}, []string{
			`mkdir -p "$(dirname -- "$HOME"/'.config/x')"`, `printf %s 'eA==' | base64 -d > "$HOME"/'.config/x'`}},
		{"cmd done", &cmd{script: "make", unless: "true"}, result{out: "0\n"}, nil},
		{"cmd to run", &cmd{script: "make", unless: "false"}, result{out: "1\n"}, []string{"make"}},
		{"new repo", &repo{path: "~/src/m", url: "u", ref: "main"}, result{}, []string{
			`GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new' git clone -q -b 'main' 'u' "$HOME"/'src/m'`}},
		{"repo ok", &repo{path: "/m", url: "u", ref: "main"}, result{out: "u\nmain\n"}, nil},
		{"repo at a commit", &repo{path: "/m", url: "u", ref: "abc123"}, result{out: "u\nabc1234567\n"}, nil},
		{"repo moved", &repo{path: "/m", url: "v", ref: "dev"}, result{out: "u\nmain\n"}, []string{
			"git -C '/m' remote set-url origin 'v'",
			"GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new' git -C '/m' fetch -q origin",
			"git -C '/m' checkout -q 'dev'"}},
		{"new key", &key{}, result{}, []string{`install -d -m 700 "$HOME/.ssh"`,
			`ssh-keygen -q -t ed25519 -N '' -C "$(id -un)@$(hostname)" -f "$HOME/.ssh/id_ed25519"`}},
		{"key ok", &key{}, result{out: "ssh-ed25519 CCCC whiff@vps\n"}, nil},
	} {
		got, err := tc.res.check(context.Background(), &fake{results: []result{tc.out}})
		if err != nil || strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: check = %q, %v\nwant %q", tc.name, got, err, tc.want)
		}
	}
}

func TestConverge(t *testing.T) {
	h := host{dest: "admin@a", resources: []resource{
		&pkg{names: []string{"git"}, common: common{restarts: []string{"ssh"}}},
		&user{name: "whiff"},
		&dir{path: "~/src", common: common{as: "whiff", restarts: []string{"whiff"}}},
	}}

	// A plan reports a check that fails after a change, and goes on.
	var out strings.Builder
	f := &fake{results: []result{{out: "git\n"}, {out: "\n"}, {code: 1, stderr: "sudo: unknown user whiff"}}}
	if err := converge(context.Background(), &out, f, h, false); err != nil {
		t.Fatal(err)
	}
	want := `admin@a
  change  pkg git
            apt-get update -q
            DEBIAN_FRONTEND=noninteractive apt-get install -y -q 'git'
  change  user whiff
            useradd -m -s '/bin/bash' 'whiff'
  new     dir ~/src (as whiff)
  restart ssh, whiff (as whiff)
`
	if out.String() != want {
		t.Errorf("plan:\n%s\nwant:\n%s", out.String(), want)
	}
	if len(f.ran) != 3 {
		t.Errorf("plan ran %d scripts, want 3 checks", len(f.ran))
	}

	// apply fixes, checks again, and restarts at the end.
	f = &fake{results: []result{
		{out: "git\n"}, {}, {}, // pkg: check, fix, check
		{out: "/bin/bash\nwhiff\n"},  // user: ok
		{}, {}, {out: "whiff 755\n"}, // dir: check (missing), fix, check
		{}, // restart
	}}
	out.Reset()
	if err := converge(context.Background(), &out, f, h, true); err != nil {
		t.Fatal(err)
	}
	if ssh := f.ran[len(f.ran)-2]; ssh != "$ set -e\nsystemctl daemon-reload\nsystemctl restart 'ssh'" {
		t.Errorf("next to last script = %q, want ssh's restart", ssh)
	}
	if f.ran[5] != "whiff$ set -e\nmkdir -p \"$HOME\"/'src'" {
		t.Errorf("dir fix = %q", f.ran[5])
	}

	// apply stops at a resource still off after its fix.
	f = &fake{results: []result{{out: "git\n"}, {}, {out: "git\n"}}}
	if err := converge(context.Background(), &out, f, h, true); err == nil || !strings.Contains(err.Error(), "pkg git: still differs") {
		t.Errorf("apply = %v, want still differs", err)
	}
}

func TestRunUsage(t *testing.T) {
	var stderr strings.Builder
	if code := run(context.Background(), []string{"diff", "x"}, nil, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "usage") {
		t.Errorf("run diff = %d %q", code, stderr.String())
	}
}

// TestSetup reads a user's setup as resources run as them, in the order
// written, with every pkg first, together.
func TestSetup(t *testing.T) {
	path := write(t, `
(def me ["ssh-ed25519 AAAA me"])
(def (dev name) (user name {:keys me} [(pkg "git") (ssh-key)]))
(host "admin@a"
  [(pkg "curl" "git")
   (user "whiff" {:keys me :linger #t}
     [(pkg "golang-go")
      (dir "~/src")
      (file "~/.ssh/config" {:from "dotfiles/ssh" :mode "600" :restarts "x"})
      [(repo "~/src/m" "git@github.com:me/m.git" {:ref "main"})
       (go-bin "golang.org/x/tools/gopls@v0.20.0")]
      (env "~/.env" {"KEY" {:cmd "echo k"}})
      (cmd "make" {:unless "true" :name "build"})
      (dir "/srv" {:as "root"})])
   (dev "ana")
   (file "/etc/motd" "hi" {:restarts "ssh"})])`)
	hosts, err := load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range hosts[0].resources {
		got = append(got, r.String()+" "+r.opts().as+" "+fmt.Sprint(r.opts().restarts))
	}
	want := `pkg curl git golang-go  []
user whiff  []
dir ~/src whiff []
file ~/.ssh/config whiff [x]
repo ~/src/m whiff []
go-bin golang.org/x/tools/gopls@v0.20.0 whiff []
env ~/.env whiff []
cmd build whiff []
dir /srv root []
user ana  []
ssh-key ~/.ssh/id_ed25519 ana []
file /etc/motd  [ssh]`
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("got:\n%s\nwant:\n%s", g, want)
	}
	rs := hosts[0].resources
	if u := rs[1].(*user); !u.linger {
		t.Error("whiff doesn't linger")
	}
	if f := rs[3].(*file); f.from != filepath.Join(filepath.Dir(path), "dotfiles/ssh") || f.mode != "600" {
		t.Errorf("ssh config from %s mode %s", f.from, f.mode)
	}

	for _, tc := range []struct{ src, err string }{
		{`(host "a" {:users {}})`, "want host's setup as a vector"},
		{`(host "a" [(user "u" [(user "v")])])`, "user u: a user's setup can't hold another user, as v"},
		{`(host "a" [(user "u" [1])])`, "user u: want a resource, got 1"},
		{`(host "a" [(user "u" {:linger "yes"})])`, ":linger: want #t or #f"},
		{`(host "a" [(env "~/.env" {"K" "secret"})])`, "a value never goes in the file"},
		{`(host "a" [(env "~/.env" {"K" {:env "A" :cmd "b"}})])`, "want one of :env and :cmd"},
		{`(host "a" [(env "~/.env" {"A-B" {:env "A"}})])`, "isn't a variable name"},
		{`(host "a" [(file "~/x" {:mode "600"})])`, "file takes a path, its content"},
		{`(host "a" [(go-bin "golang.org/x/tools/gopls")])`, "go-bin: want pkg@version"},
		{`(host "a" [(ssh-key "x")])`, "ssh-key takes only options"},
	} {
		if _, err := load(context.Background(), write(t, tc.src)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("load %s = %v, want %q", tc.src, err, tc.err)
		}
	}
}

// TestSecrets reads an env file's values here and sends them on stdin:
// neither the plan nor any script holds one.
func TestSecrets(t *testing.T) {
	t.Setenv("WAFT_TEST_KEY", `s3cr"t`)
	e := &env{common: common{as: "whiff"}, path: "~/.whiff/env",
		vars: []envVar{{name: "A", from: "WAFT_TEST_KEY"}, {name: "B", cmd: `printf 'tw\\o\n'`}}}
	h := host{dest: "admin@a", resources: []resource{e}}
	f := &fake{results: []result{{}, {}, {out: "x\n600\n"}}}
	var out strings.Builder
	if err := converge(context.Background(), &out, f, h, true); err == nil || !strings.Contains(err.Error(), "still differs") {
		t.Fatalf("apply = %v", err)
	}
	if want := "A=\"s3cr\\\"t\"\nB=\"tw\\\\o\"\n"; f.inputs[1] != want {
		t.Errorf("stdin = %q, want %q", f.inputs[1], want)
	}
	for _, s := range append(f.ran, out.String()) {
		if strings.Contains(s, "s3cr") || strings.Contains(s, "tw") {
			t.Errorf("a secret in %q", s)
		}
	}
	if !strings.Contains(out.String(), "# write ~/.whiff/env: A B") {
		t.Errorf("plan:\n%s", out.String())
	}

	e = &env{path: "/e", vars: []envVar{{name: "A", from: "WAFT_TEST_UNSET"}}}
	if _, err := e.check(context.Background(), &fake{}); err == nil || !strings.Contains(err.Error(), "$WAFT_TEST_UNSET isn't set here") {
		t.Errorf("unset: %v", err)
	}
}

// TestGo installs a program that's missing or at another version, and
// leaves one at the version, or one at latest, as it is.
func TestGo(t *testing.T) {
	built := func(version string) result {
		return result{out: "/home/u/go/bin/gopls: go1.27.0\n\tpath\tgolang.org/x/tools/gopls\n\tmod\tgolang.org/x/tools/gopls\t" + version + "\th1:x=\n"}
	}
	install := []string{"go install 'golang.org/x/tools/gopls@v0.20.0'"}
	for _, tc := range []struct {
		name, version string
		out           result
		want          []string
	}{
		{"missing", "v0.20.0", result{}, install},
		{"same", "v0.20.0", built("v0.20.0"), nil},
		{"older", "v0.20.0", built("v0.19.0"), install},
		{"latest", "latest", built("v0.19.0"), nil},
		{"commit", "abc1234", built("v0.0.0-20261109120000-abc1234def56"), nil},
		{"other commit", "abc1234", built("v0.0.0-20261109120000-fff1234def56"), []string{"go install 'golang.org/x/tools/gopls@abc1234'"}},
	} {
		g := &goBin{pkg: "golang.org/x/tools/gopls", version: tc.version}
		got, err := g.check(context.Background(), &fake{results: []result{tc.out}})
		if err != nil || strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: check = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestUserRestart restarts a unit a user's resource names as that user,
// after the system's.
func TestUserRestart(t *testing.T) {
	h := host{dest: "admin@a", resources: []resource{
		&file{path: "/etc/x", content: "x", common: common{restarts: []string{"ssh"}}},
		&file{path: "~/u", content: "x", common: common{as: "whiff", restarts: []string{"whiff"}}},
	}}
	hash := "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	f := &fake{results: []result{{}, {}, {out: hash + "\nroot 644\n"}, {}, {}, {out: hash + "\nwhiff 644\n"}}}
	var out strings.Builder
	if err := converge(context.Background(), &out, f, h, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  restart ssh, whiff (as whiff)\n") {
		t.Errorf("plan:\n%s", out.String())
	}
	n := len(f.ran)
	if f.ran[n-2] != "$ set -e\nsystemctl daemon-reload\nsystemctl restart 'ssh'" ||
		f.ran[n-1] != "whiff$ set -e\n"+userBus+"\nsystemctl --user daemon-reload\nsystemctl --user restart 'whiff'" {
		t.Errorf("restarts:\n%s\n%s", f.ran[n-2], f.ran[n-1])
	}
}

// TestRemote runs a script as its user, in its home.
func TestRemote(t *testing.T) {
	if got, want := remote("whiff", "go install x"), `sudo -n -H -u 'whiff' -- sh -c 'cd "$HOME" || exit 1
go install x'`; got != want {
		t.Errorf("remote = %s\nwant %s", got, want)
	}
}
