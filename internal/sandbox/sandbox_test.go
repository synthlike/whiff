package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"runtime"
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
