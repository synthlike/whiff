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
