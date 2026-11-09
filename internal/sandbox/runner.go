package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// A Runner makes the command that runs argv in dir: on the host, or in a
// container that sees only dir. The caller configures the command (its
// streams, process group, Cancel) and runs it, as it would exec's. What
// the caller puts in its Env reaches the process the Runner starts, which
// for a container is the runtime's client, not the command inside.
//
// name is what the runtime knows the command by, for a container: one the
// caller makes with Name and keeps, so that what a killed client left
// running can be found and ended by it (Gone, End). Direct ignores it.
type Runner interface {
	Command(ctx context.Context, name, dir string, argv []string) *exec.Cmd
	// Gone says whether nothing named name runs any more.
	Gone(ctx context.Context, name string) bool
	// End ends and deletes what runs as name, if anything does.
	End(ctx context.Context, name string)
}

// Name is a new name for a sandboxed command: whiff-<what>-<random>, so
// no other's can be the same, as a pid can.
func Name(what string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return "whiff-" + what + "-" + hex.EncodeToString(b)
}

// Direct runs commands as they are, on the host.
type Direct struct{}

func (Direct) Command(ctx context.Context, _, dir string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd
}

func (Direct) Gone(context.Context, string) bool { return true }
func (Direct) End(context.Context, string)       {}

// cacheVolume is the runtime's named volume Go's build cache is kept in,
// across sandboxed commands: not a host directory.
const cacheVolume = "whiff-gocache:/root/.cache/go-build"

// Container runs each command in a new container of Image, removed as it
// ends, that sees only its dir, at the same path, so paths in its output
// are the host's, and, if dir is a git worktree, its repository's .git
// read-only (gitDirs). Go's module cache, if the host has one, is there
// read-only, so a build doesn't need the network for modules it has.
type Container struct {
	Backend
	Net      bool   // the command may reach the network, and the host's servers
	ModCache string // the host's Go module cache, or ""
}

// New is the Container for this host, or why there can't be one: no
// runtime, one that isn't running, no image or no network to run without.
// Nothing then runs unconfined: the caller fails.
func New(ctx context.Context, net bool) (*Container, error) {
	b, err := Find(runtime.GOOS, exec.LookPath)
	if err != nil {
		return nil, err
	}
	if err := b.Ready(ctx); err != nil {
		return nil, err
	}
	if !b.HasImage(ctx) {
		return nil, fmt.Errorf("the sandbox's image, %s, isn't built: whiff sandbox build", Image)
	}
	if !net && !b.HasNetwork(ctx) {
		return nil, fmt.Errorf("the sandbox's network %s isn't there: whiff sandbox build makes it", noNet)
	}
	return &Container{Backend: b, Net: net, ModCache: modCache()}, nil
}

// Host is a Container for this host's runtime, to find and end commands by
// name, as after whiff died: it doesn't check it can run new ones.
func Host() (*Container, error) {
	b, err := Find(runtime.GOOS, exec.LookPath)
	if err != nil {
		return nil, err
	}
	return &Container{Backend: b}, nil
}

// modCache is the host's Go module cache, if there is one.
func modCache() string {
	dir := os.Getenv("GOMODCACHE")
	if dir == "" {
		gopath := os.Getenv("GOPATH")
		if gopath == "" {
			home, _ := os.UserHomeDir()
			gopath = filepath.Join(home, "go")
		}
		dir = filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// gitDirs is what a worktree at dir needs mounted for git to work in it:
// its .git is a file naming its gitdir (<repo>/.git/worktrees/<name>),
// which names the repository's own .git as commondir. Both are outside
// dir, so a container would see the worktree as no repository. They go
// in read-only at the paths git finds them by: status, diff and log work,
// while nothing in the container can write hooks, config or refs the host
// runs or trusts. A commit fails, as it can't lock the index; whiff
// commits on the host. None for a repository's own checkout, whose .git
// is in dir, or outside one.
func gitDirs(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return nil // no .git, or a directory, in dir
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return nil
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common = strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		common = filepath.Clean(common)
	}
	// Not inside dir, which is mounted already, and not over it.
	var dirs []string
	for _, d := range []string{common, gitdir} {
		if !within(d, dir) && !within(dir, d) && (len(dirs) == 0 || !within(d, dirs[0])) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// within says whether path is dir or in it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// endGrace is how long a container's command has to end after SIGTERM.
const endGrace = 5 * time.Second

// Args is the runtime's argv that runs argv in dir, as name.
func (c *Container) Args(name, dir string, argv []string) []string {
	args := append([]string{c.Name, "run"}, c.Run...)
	args = append(args, "--rm", "--init", "--name", name, "-v", dir+":"+dir, "-w", dir, "-v", cacheVolume)
	for _, d := range gitDirs(dir) {
		args = append(args, "-v", d+":"+d+":ro")
	}
	if c.ModCache != "" {
		args = append(args, "-v", c.ModCache+":/go/pkg/mod:ro")
	}
	if !c.Net {
		args = append(args, c.NoNet...)
	}
	args = append(args, Image)
	return append(args, argv...)
}

// Command runs argv in a container. Cancelling ctx sends the runtime's
// client SIGTERM, which --init passes on to the command; a client that
// doesn't end in endGrace is killed. Either may leave the container
// running (Apple's client ends without passing SIGTERM on while its VM
// boots), so after a cancel, End it.
func (c *Container) Command(ctx context.Context, name, dir string, argv []string) *exec.Cmd {
	args := c.Args(name, dir, argv)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = endGrace
	return cmd
}

func (c *Container) Gone(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, c.Inspect[0], append(c.Inspect[1:], name)...).Run() != nil
}

func (c *Container) End(ctx context.Context, name string) {
	exec.CommandContext(ctx, c.Remove[0], append(c.Remove[1:], name)...).Run()
}
