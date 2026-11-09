package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if changed, err := converge(context.Background(), &out, f, h, false); err != nil || !changed {
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
	if _, err := converge(context.Background(), &out, f, h, true); err != nil {
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
	if _, err := converge(context.Background(), &out, f, h, true); err == nil || !strings.Contains(err.Error(), "pkg git: still differs") {
		t.Errorf("apply = %v, want still differs", err)
	}
}

func TestRunUsage(t *testing.T) {
	var stderr strings.Builder
	if code := run(context.Background(), []string{"diff", "x"}, nil, &stderr, nil); code != 1 || !strings.Contains(stderr.String(), "usage") {
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
	if _, err := converge(context.Background(), &out, f, h, true); err == nil || !strings.Contains(err.Error(), "still differs") {
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
	if _, err := converge(context.Background(), &out, f, h, true); err != nil {
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

// TestWhiffLib uses lib/whiff.puff, copied next to a host file, as the
// usage says: whiff for a user is plain resources run as them.
func TestWhiffLib(t *testing.T) {
	lib, err := os.ReadFile("lib/whiff.puff")
	if err != nil {
		t.Fatal(err)
	}
	path := write(t, `(use "whiff.puff")
(host "admin@a"
  [(pkg "golang-go")
   (user "whiff" {:linger #t}
     [(whiff {:version "v0.8.0"
              :env {"KEY" {:env "KEY"}}
              :services ["services/pipeline.puff"]})])])`)
	os.WriteFile(filepath.Join(filepath.Dir(path), "whiff.puff"), lib, 0o644)
	hosts, err := load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range hosts[0].resources {
		got = append(got, r.String()+" "+r.opts().as+" "+fmt.Sprint(r.opts().restarts))
	}
	want := `pkg golang-go  []
user whiff  []
go-bin github.com/synthlike/whiff/cmd/whiff@v0.8.0 whiff [whiff]
env ~/.whiff/env whiff [whiff]
file ~/.config/systemd/user/whiff.service whiff [whiff]
file ~/.whiff/services/pipeline.puff whiff []
cmd whiff enabled and running whiff []`
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("got:\n%s\nwant:\n%s", g, want)
	}
	rs := hosts[0].resources
	if f := rs[4].(*file); !strings.Contains(f.content, "ExecStart=%h/go/bin/whiff daemon\nEnvironmentFile=-%h/.whiff/env\n") {
		t.Errorf("unit:\n%s", f.content)
	}
	if f := rs[5].(*file); f.from != filepath.Join(filepath.Dir(path), "services/pipeline.puff") {
		t.Errorf("service from %s", f.from)
	}
	if c := rs[6].(*cmd); !strings.HasPrefix(c.script, userBus+"\n") || !strings.HasPrefix(c.unless, userBus+";") {
		t.Errorf("cmd %q unless %q", c.script, c.unless)
	}
}

// TestRemote runs a script as its user, in its home.
func TestRemote(t *testing.T) {
	if got, want := remote("whiff", "go install x"), `sudo -n -H -u 'whiff' -- sh -c 'cd "$HOME" || exit 1
go install x'`; got != want {
		t.Errorf("remote = %s\nwant %s", got, want)
	}
}

// TestShort shows a write as its path and size, and cuts a long line.
func TestShort(t *testing.T) {
	for cmd, want := range map[string]string{
		writeCmd("hello\n", shPath("~/.config/x")):    "write ~/.config/x (6 bytes)",
		writeCmd("k", `"$home/.ssh/authorized_keys"`): "write $home/.ssh/authorized_keys (1 byte)",
		"make\nmake install":                          "make...",
		strings.Repeat("x", 120):                      strings.Repeat("x", 97) + "...",
		"printf %s 'not base64!' | base64 -d > /x":    "printf %s 'not base64!' | base64 -d > /x",
	} {
		if got := short(cmd); got != want {
			t.Errorf("short(%.30q) = %q, want %q", cmd, got, want)
		}
	}
}

// TestDrift exits 2 from a plan when a host differs, 0 when it matches.
func TestDrift(t *testing.T) {
	path := write(t, `(host "admin@a" [(pkg "git")])`)
	for _, tc := range []struct {
		out  string
		want int
	}{{"", 0}, {"git\n", 2}} {
		dial := func(string) (runner, func(), error) {
			return &fake{results: []result{{out: tc.out}}}, func() {}, nil
		}
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"plan", path}, &stdout, &stderr, dial); code != tc.want {
			t.Errorf("plan with %q missing = %d, want %d: %s", tc.out, code, tc.want, stderr.String())
		}
	}
}

// TestLine adds a line, puts it in place of the lines :match finds, and
// leaves a file that has it as it is.
func TestLine(t *testing.T) {
	m := regexp.MustCompile(`^#?PasswordAuthentication`)
	for _, tc := range []struct {
		name, content string
		match         *regexp.Regexp
		want          string
	}{
		{"empty", "", nil, "PasswordAuthentication no\n"},
		{"there", "a\nPasswordAuthentication no\n", nil, "a\nPasswordAuthentication no\n"},
		{"added", "a\n", nil, "a\nPasswordAuthentication no\n"},
		{"replaced", "#PasswordAuthentication yes\nb\nPasswordAuthentication yes\n", m,
			"PasswordAuthentication no\nb\nPasswordAuthentication no\n"},
		{"no match", "b\n", m, "b\nPasswordAuthentication no\n"},
	} {
		if got := withLine(tc.content, "PasswordAuthentication no", tc.match); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	l := &line{path: "/etc/x", text: "b", match: regexp.MustCompile("^b")}
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"missing", "", []string{`mkdir -p "$(dirname -- '/etc/x')"`, writeCmd("b\n", "'/etc/x'")}},
		{"empty", ".\n", []string{writeCmd("b\n", "'/etc/x'")}},
		{"ok", ".\na\nb\n", nil},
		{"differs", ".\nbb\n", []string{writeCmd("b\n", "'/etc/x'")}},
	} {
		got, err := l.check(context.Background(), &fake{results: []result{{out: tc.out}}})
		if err != nil || strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: check = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ src, err string }{
		{`(host "a" [(line "/x" "a\nb")])`, "is more than a line"},
		{`(host "a" [(line "/x" "a" {:match "("})])`, "line's :match"},
		{`(host "a" [(file "/x" "a" {:valid "true"})])`, ":valid: want %s in it"},
	} {
		if _, err := load(context.Background(), write(t, tc.src)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("load %s = %v, want %q", tc.src, err, tc.err)
		}
	}
}

// TestValid writes a file beside its place and moves it in only if :valid
// passes on it; a failing check stops apply with its output.
func TestValid(t *testing.T) {
	f := &file{path: "/etc/ssh/sshd_config.d/00-x.conf", content: "a\n", valid: "sshd -t -f %s", common: common{restarts: []string{"ssh"}}}
	p := "'/etc/ssh/sshd_config.d/00-x.conf'"
	fix, err := f.check(context.Background(), &fake{results: []result{{out: "abc\nroot 644\n"}}})
	want := []string{"[ ! -f " + p + " ] || cp -p " + p + " " + p + ".waft",
		writeCmd("a\n", p+".waft"),
		"sshd -t -f " + p + ".waft || { rm -f " + p + ".waft; exit 1; }",
		"mv " + p + ".waft " + p}
	if err != nil || strings.Join(fix, "\n") != strings.Join(want, "\n") {
		t.Errorf("check = %q %v\nwant %q", fix, err, want)
	}

	h := host{dest: "admin@a", resources: []resource{f}}
	run := &fake{results: []result{{out: "abc\nroot 644\n"}, {code: 1, stderr: "line 1: Bad configuration option: a"}}}
	var out strings.Builder
	if _, err := converge(context.Background(), &out, run, h, true); err == nil || !strings.Contains(err.Error(), "Bad configuration option") {
		t.Errorf("apply = %v", err)
	}
	if len(run.ran) != 2 {
		t.Errorf("ran %d scripts after a failing check, want 2: no restart", len(run.ran))
	}
}

// TestHardenedLib uses lib/hardened.puff as its comments say.
func TestHardenedLib(t *testing.T) {
	lib, err := os.ReadFile("lib/hardened.puff")
	if err != nil {
		t.Fatal(err)
	}
	path := write(t, `(use "hardened.puff")
(host "admin@a" [(hardened {:allow ["80/tcp"] :root-login "no"})])`)
	os.WriteFile(filepath.Join(filepath.Dir(path), "hardened.puff"), lib, 0o644)
	hosts, err := load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range hosts[0].resources {
		got = append(got, r.String()+" "+fmt.Sprint(r.opts().restarts))
	}
	want := `pkg ufw unattended-upgrades []
file /etc/ssh/sshd_config.d/00-hardened.conf [ssh]
file /etc/apt/apt.conf.d/20auto-upgrades []
cmd ufw allow OpenSSH []
cmd ufw allow 80/tcp []
cmd ufw --force enable []`
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("got:\n%s\nwant:\n%s", g, want)
	}
	rs := hosts[0].resources
	if f := rs[1].(*file); f.content != "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\n" || !strings.Contains(f.valid, "sshd -t -f %s") {
		t.Errorf("sshd config %q valid %q", f.content, f.valid)
	}
	if c := rs[4].(*cmd); c.unless != "ufw show added | grep -qxF 'ufw allow 80/tcp'" {
		t.Errorf("unless %q", c.unless)
	}
}

// TestAbsent removes what's there and leaves what isn't; a user goes after
// what it runs.
func TestAbsent(t *testing.T) {
	gone := func(c common) common { c.absent = true; return c }
	for _, tc := range []struct {
		name string
		res  resource
		out  result
		want []string
	}{
		{"file there", &file{common: gone(common{}), path: "/etc/motd"}, result{out: "y\n"}, []string{"rm -f '/etc/motd'"}},
		{"file gone", &file{common: gone(common{}), path: "/etc/motd"}, result{}, nil},
		{"dir", &dir{common: gone(common{}), path: "~/old"}, result{out: "y\n"}, []string{`rm -r "$HOME"/'old'`}},
		{"repo", &repo{common: gone(common{}), path: "~/src/m"}, result{out: "y\n"}, []string{`rm -rf "$HOME"/'src/m'`}},
		{"env", &env{common: gone(common{}), path: "~/.env"}, result{out: "y\n"}, []string{`rm -f "$HOME"/'.env'`}},
		{"go-bin", &goBin{common: gone(common{}), pkg: "golang.org/x/tools/gopls"}, result{out: "y\n"}, []string{`rm -f "$HOME"/go/bin/'gopls'`}},
		{"ssh-key", &key{common: gone(common{})}, result{out: "y\n"}, []string{`rm -f "$HOME/.ssh/id_ed25519" "$HOME/.ssh/id_ed25519.pub"`}},
		{"pkg", &pkg{common: gone(common{}), names: []string{"telnet", "ftp"}}, result{out: "ftp\n"},
			[]string{"DEBIAN_FRONTEND=noninteractive apt-get remove -y -q 'ftp'"}},
		{"pkg gone", &pkg{common: gone(common{}), names: []string{"telnet"}}, result{}, nil},
		{"line", &line{common: gone(common{}), path: "/x", text: "b", match: regexp.MustCompile("^#?b")}, result{out: ".\na\n#b\nb\n"},
			[]string{writeCmd("a\n", "'/x'")}},
		{"line gone", &line{common: gone(common{}), path: "/x", text: "b"}, result{out: ".\na\n"}, nil},
		{"line, no file", &line{common: gone(common{}), path: "/x", text: "b"}, result{}, nil},
		{"user gone", &user{common: gone(common{}), name: "old"}, result{}, nil},
	} {
		got, err := tc.res.check(context.Background(), &fake{results: []result{tc.out}})
		if err != nil || strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: check = %q, %v\nwant %q", tc.name, got, err, tc.want)
		}
	}
	u := &user{common: gone(common{}), name: "old"}
	fix, _ := u.check(context.Background(), &fake{results: []result{{out: "y\n"}}})
	if len(fix) != 5 || fix[0] != "loginctl disable-linger 'old' 2>/dev/null || true" || fix[4] != "userdel -r 'old' || ! getent passwd 'old' >/dev/null" {
		t.Errorf("user fix = %q", fix)
	}

	hosts, err := load(context.Background(), write(t, `(host "admin@a"
  [(pkg "git") (pkg "telnet" {:absent #t}) (file "/etc/motd" {:absent #t}) (user "old" {:absent #t})
   (repo "~/src/m" {:absent #t}) (env "~/.env" {:absent #t})])`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range hosts[0].resources {
		names = append(names, fmt.Sprint(r, r.opts().absent))
	}
	if g := strings.Join(names, "|"); g != "pkg git false|pkg telnet true|file /etc/motd true|user old true|repo ~/src/m true|env ~/.env true" {
		t.Errorf("resources: %s", g)
	}
	h := host{dest: "admin@a", resources: hosts[0].resources[2:3]}
	var out strings.Builder
	converge(context.Background(), &out, &fake{results: []result{{out: "y\n"}}}, h, false)
	if !strings.Contains(out.String(), "  change  file /etc/motd (absent)\n            rm -f '/etc/motd'\n") {
		t.Errorf("plan:\n%s", out.String())
	}

	for _, tc := range []struct{ src, err string }{
		{`(host "a" [(cmd "x" {:unless "true" :absent #t})])`, "cmd can't be :absent"},
		{`(host "a" [(user "u" {:absent #t} [(dir "~/x")])])`, "user u is :absent"},
		{`(host "a" [(pkg "git") (pkg "git" {:absent #t})])`, "pkg git is both installed and :absent"},
		{`(host "a" [(file "/x" "" {:absent "yes"})])`, ":absent: want #t or #f"},
		{`(host "a" [(file "/x")])`, "file takes 2 to 3 arguments"},
		{`(host "a" [(repo "/x")])`, "repo takes 2 to 3 arguments"},
		{`(host "a" [(env "/x" {:as "u"})])`, "env takes a path, its vars"},
	} {
		if _, err := load(context.Background(), write(t, tc.src)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("load %s = %v, want %q", tc.src, err, tc.err)
		}
	}
}
