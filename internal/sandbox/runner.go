package sandbox

import (
	"context"
	"os/exec"
)

// A Runner makes the command that runs argv in dir: on the host, or in a
// container that sees only dir. The caller configures the command (its
// streams, process group, Cancel) and runs it, as it would exec's. What
// the caller puts in its Env reaches the process the Runner starts, which
// for a container is the runtime's client, not the command inside.
type Runner interface {
	Command(ctx context.Context, dir string, argv []string) *exec.Cmd
}

// Direct runs commands as they are, on the host.
type Direct struct{}

func (Direct) Command(ctx context.Context, dir string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd
}
