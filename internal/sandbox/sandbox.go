// Package sandbox finds the container runtime whiff confines commands
// with, and builds the image they run in: Apple's container on macOS,
// rootless Podman on Linux.
package sandbox

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed Containerfile
var Containerfile string

// Image is the image's name and tag: the tag is a hash of Containerfile,
// so a whiff whose Containerfile changed asks for a new build.
var Image = "whiff-sandbox:" + hash(Containerfile)

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

// Backend is a container runtime: its name and the commands whiff runs it
// with. The build and image commands are the same for both runtimes.
type Backend struct {
	Name   string   // "container" or "podman", also its binary
	Status []string // argv that fails while the runtime can't run containers
	Start  string   // what to run when Status fails
	// NoNet is run's flags for a container without a network. Apple's
	// container has no network "none": an --internal network, made by
	// Network, reaches neither the internet nor the host.
	NoNet   []string
	Network []string // argv that makes NoNet's network, if it needs one
	Run     []string // run's own flags, as Apple's to keep progress out of output
	Inspect []string // argv, before a name, that fails if no container has it
	Remove  []string // argv, before a name, that ends and deletes its container
	// Volume is the named volume Go's build cache is kept in, or "" for
	// a host directory: an Apple volume is a disk image one VM at a time
	// can attach, so a second sandboxed command while one runs wouldn't
	// start.
	Volume string
}

// noNet is the network Apple's container runs commands without a network on.
const noNet = "whiff-nonet"

var backends = map[string]Backend{
	"darwin": {
		Name:    "container",
		Status:  []string{"container", "system", "status"},
		Start:   "container system start",
		NoNet:   []string{"--network", noNet},
		Network: []string{"container", "network", "create", "--internal", noNet},
		Run:     []string{"--progress", "none"},
		Inspect: []string{"container", "inspect"},
		Remove:  []string{"container", "delete", "--force"},
	},
	"linux": {
		Name:    "podman",
		Status:  []string{"podman", "info"},
		Start:   "podman info, as the user whiff runs as, for what's wrong",
		NoNet:   []string{"--network", "none"},
		Inspect: []string{"podman", "container", "inspect"},
		Remove:  []string{"podman", "rm", "--force", "--time", "0"},
		Volume:  "whiff-gocache",
	},
}

// Find returns the backend for goos, if its binary is on PATH (lookPath,
// exec.LookPath outside tests), or an error that says what to install.
func Find(goos string, lookPath func(string) (string, error)) (Backend, error) {
	b, ok := backends[goos]
	if !ok {
		return Backend{}, fmt.Errorf("no sandbox on %s: whiff sandboxes with Apple's container on macOS and Podman on Linux", goos)
	}
	if _, err := lookPath(b.Name); err != nil {
		install := "brew install container, then container system start --enable-kernel-install"
		if b.Name == "podman" {
			install = "apt install podman uidmap"
		}
		return Backend{}, fmt.Errorf("no %s on PATH: %s", b.Name, install)
	}
	return b, nil
}

// Version is the runtime's own version line.
func (b Backend) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, b.Name, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", b.Name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Ready says why the runtime can't run containers now, or nil.
func (b Backend) Ready(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, b.Status[0], b.Status[1:]...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return fmt.Errorf("%s isn't ready (%s): %s", b.Name, msg, b.Start)
	}
	return nil
}

// HasNetwork says whether NoNet's network is there, or needs none.
func (b Backend) HasNetwork(ctx context.Context) bool {
	return b.Network == nil || exec.CommandContext(ctx, b.Name, "network", "inspect", noNet).Run() == nil
}

// MakeNetwork makes NoNet's network, if it needs one that isn't there.
func (b Backend) MakeNetwork(ctx context.Context) error {
	if b.HasNetwork(ctx) {
		return nil
	}
	out, err := exec.CommandContext(ctx, b.Network[0], b.Network[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(b.Network, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HasImage says whether Image has been built.
func (b Backend) HasImage(ctx context.Context) bool {
	return exec.CommandContext(ctx, b.Name, "image", "inspect", Image).Run() == nil
}

// BuildArgs is the argv that builds Image from a directory holding
// Containerfile.
func (b Backend) BuildArgs(dir string) []string {
	return []string{b.Name, "build", "-t", Image, "-f", filepath.Join(dir, "Containerfile"), dir}
}

// Build writes Containerfile to a temporary directory and builds Image
// there, with the runtime's output on stdout and stderr.
func (b Backend) Build(ctx context.Context, stdout, stderr io.Writer) error {
	dir, err := os.MkdirTemp("", "whiff-sandbox-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(Containerfile), 0o644); err != nil {
		return err
	}
	argv := b.BuildArgs(dir)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s build: %v", b.Name, err)
	}
	return nil
}
