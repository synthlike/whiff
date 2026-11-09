package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	found := func(string) (string, error) { return "/bin/x", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, c := range []struct {
		goos     string
		lookPath func(string) (string, error)
		name     string // the backend found, or "" for an error
		err      string // what the error says
	}{
		{"darwin", found, "container", ""},
		{"linux", found, "podman", ""},
		{"darwin", missing, "", "brew install container"},
		{"linux", missing, "", "apt install podman"},
		{"windows", found, "", "no sandbox on windows"},
	} {
		b, err := Find(c.goos, c.lookPath)
		if c.name != "" {
			if err != nil || b.Name != c.name {
				t.Errorf("Find(%s) = %q, %v; want %s", c.goos, b.Name, err, c.name)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("Find(%s) err = %v; want it to say %q", c.goos, err, c.err)
		}
	}
}

func TestImage(t *testing.T) {
	if !regexp.MustCompile(`^whiff-sandbox:[0-9a-f]{12}$`).MatchString(Image) {
		t.Errorf("Image = %q", Image)
	}
	if hash(Containerfile+" ") == hash(Containerfile) {
		t.Error("a changed Containerfile keeps its tag")
	}
	// Without the network, go must use the image's toolchain.
	if !strings.Contains(Containerfile, "GOTOOLCHAIN=local") {
		t.Error("Containerfile doesn't set GOTOOLCHAIN=local")
	}
	b := backends["linux"]
	got := strings.Join(b.BuildArgs("/tmp/d"), " ")
	if want := "podman build -t " + Image + " -f /tmp/d/Containerfile /tmp/d"; got != want {
		t.Errorf("BuildArgs = %q; want %q", got, want)
	}
}

// TestImageRuns runs the built image where there is one: the toolchain a
// sandboxed command gets.
func TestImageRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := Find(runtime.GOOS, exec.LookPath)
	if err != nil || b.Ready(ctx) != nil || !b.HasImage(ctx) {
		t.Skip("no runtime, or the image isn't built: whiff sandbox build")
	}
	out, err := exec.CommandContext(ctx, b.Name, "run", "--rm", Image,
		"sh", "-c", "go version && git --version && node --version && go env GOTOOLCHAIN").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{"go version go1.27", "git version", "v20.", "local"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestArgs(t *testing.T) {
	c := &Container{Backend: backends["linux"], ModCache: "/home/u/go/pkg/mod"}
	got := strings.Join(c.Args("whiff-job-1", "/src/p", []string{"go", "test"}), " ")
	want := "podman run --rm --init --name whiff-job-1 -v /src/p:/src/p -w /src/p -v " + cacheVolume +
		" -v /home/u/go/pkg/mod:/go/pkg/mod:ro --network none " + Image + " go test"
	if got != want {
		t.Errorf("Args =\n%s\nwant\n%s", got, want)
	}
	c = &Container{Backend: backends["darwin"], Net: true}
	if got := strings.Join(c.Args("n", "/d", []string{"ls"}), " "); strings.Contains(got, "--network") || strings.Contains(got, ":ro") {
		t.Errorf("with the network and no module cache, Args = %s", got)
	}
	if got := strings.Join((&Container{Backend: backends["darwin"]}).Args("n", "/d", []string{"ls"}), " "); !strings.Contains(got, "--network "+noNet) {
		t.Errorf("Apple's, without the network, Args = %s", got)
	}
}

// TestContainer runs commands in a sandbox where there is one: what they
// can't reach, and that a cancel ends them and their container.
func TestContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := New(ctx, false)
	if err != nil {
		t.Skip(err)
	}
	dir, outside := t.TempDir(), t.TempDir()
	run := func(script string) (string, error) {
		out, err := c.Command(ctx, Name("test"), dir, []string{"sh", "-c", script}).CombinedOutput()
		return string(out), err
	}
	if out, err := run("echo in > inside && cat inside"); err != nil || out != "in\n" {
		t.Errorf("writing in dir = %q %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "inside")); err != nil || string(b) != "in\n" {
		t.Errorf("dir on the host has %q %v", b, err)
	}
	run("mkdir -p " + outside + " && echo out > " + outside + "/f")
	if _, err := os.Stat(filepath.Join(outside, "f")); err == nil {
		t.Error("a sandboxed command wrote outside its dir")
	}
	home, _ := os.UserHomeDir()
	if out, err := run("ls " + home + " /tmp/whiff-" + strconv.Itoa(os.Getuid())); err == nil {
		t.Errorf("home and whiff's socket directory are there:\n%s", out)
	}
	if out, err := run("wget -q -T 3 -O- http://example.com"); err == nil {
		t.Errorf("reached the network without :net:\n%s", out)
	}

	cctx, stop := context.WithCancel(ctx)
	name := Name("test")
	cmd := c.Command(cctx, name, dir, []string{"sleep", "300"})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); c.Gone(ctx, name); {
		if time.Now().After(deadline) {
			t.Fatal("the container didn't start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	began := time.Now()
	stop()
	cmd.Wait()
	if took := time.Since(began); took > endGrace+time.Second {
		t.Errorf("a cancel took %v, past the grace", took)
	}
	// As callers do after a cancel.
	c.End(ctx, name)
	if !c.Gone(ctx, name) {
		t.Error("the container outlived End")
	}
}

// git runs git in dir, failing the test if it fails.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// repo makes a repository with a commit and a worktree of it, wt, as a
// pipeline run's: relative says whether the worktree's .git names its
// gitdir by a relative path. It returns them and the repository's .git as
// git names it (macOS's temporary directories are under a symlink).
func repo(t *testing.T, relative bool) (string, string, string) {
	t.Helper()
	top := t.TempDir()
	r, wt := filepath.Join(top, "repo"), filepath.Join(top, "wt")
	git(t, top, "init", "-q", r)
	os.WriteFile(filepath.Join(r, "f"), []byte("a\n"), 0o644)
	git(t, r, "add", "f")
	git(t, r, "commit", "-qm", "one")
	git(t, r, "-c", "worktree.useRelativePaths="+strconv.FormatBool(relative), "worktree", "add", "-q", "-b", "run", wt)
	common, err := filepath.EvalSymlinks(filepath.Join(r, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if relative {
		common = filepath.Join(r, ".git")
	}
	return r, wt, common
}

func TestGitDirs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	r, wt, common := repo(t, false)
	if got := gitDirs(wt); len(got) != 1 || got[0] != common {
		t.Errorf("a worktree's gitDirs = %q; want [%s]", got, common)
	}
	if got := gitDirs(r); got != nil {
		t.Errorf("a repository's own gitDirs = %q; want none", got)
	}
	if got := gitDirs(t.TempDir()); got != nil {
		t.Errorf("outside a repository, gitDirs = %q", got)
	}
	_, wt, common = repo(t, true)
	if b, _ := os.ReadFile(filepath.Join(wt, ".git")); strings.Contains(string(b), "gitdir: /") {
		t.Skip("this git writes absolute gitdirs only")
	}
	if got := gitDirs(wt); len(got) != 1 || got[0] != common {
		t.Errorf("a relative worktree's gitDirs = %q; want [%s]", got, common)
	}
	c := &Container{Backend: backends["linux"]}
	if got := strings.Join(c.Args("n", wt, []string{"ls"}), " "); !strings.Contains(got, " -v "+common+":"+common+":ro ") {
		t.Errorf("a worktree's Args don't mount its .git read-only: %s", got)
	}
}

// TestContainerGit runs git in a sandboxed worktree where there is a
// sandbox: it reads, as a pipeline's dev and check do, and can't write
// the repository's .git.
func TestContainerGit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := New(ctx, false)
	if err != nil {
		t.Skip(err)
	}
	_, wt, common := repo(t, false)
	os.WriteFile(filepath.Join(wt, "f"), []byte("a\nb\n"), 0o644)
	out, err := c.Command(ctx, Name("test"), wt, []string{"sh", "-c",
		"git status --short && git diff --stat && git log --oneline && git rev-parse --abbrev-ref HEAD"}).CombinedOutput()
	if err != nil {
		t.Fatalf("git in a sandboxed worktree: %v\n%s", err, out)
	}
	for _, want := range []string{" M f", "1 insertion", "one", "run"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("git's output lacks %q:\n%s", want, out)
		}
	}
	hook := filepath.Join(common, "hooks", "post-checkout")
	out, err = c.Command(ctx, Name("test"), wt, []string{"sh", "-c", "touch " + hook + " && git add f"}).CombinedOutput()
	if err == nil {
		t.Errorf("wrote the repository's .git from a sandbox:\n%s", out)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Error("a sandboxed command left a hook in the repository")
	}
}
