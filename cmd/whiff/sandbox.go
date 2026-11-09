package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"

	"github.com/synthlike/whiff/internal/sandbox"
)

// sandboxCmd is whiff sandbox check and whiff sandbox build. Neither needs
// the daemon: they run the container runtime as you.
func sandboxCmd(sub string, stdout, stderr io.Writer) int {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	b, err := sandbox.Find(runtime.GOOS, exec.LookPath)
	if err == nil {
		err = b.Ready(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "whiff: %v\n", err)
		return 1
	}
	switch sub {
	case "check":
		v, err := b.Version(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "runtime  %s\n", v)
		if !b.HasImage(ctx) {
			fmt.Fprintf(stdout, "image    %s isn't built: whiff sandbox build\n", sandbox.Image)
			return 1
		}
		fmt.Fprintf(stdout, "image    %s\n", sandbox.Image)
		if !b.HasNetwork(ctx) {
			fmt.Fprintf(stdout, "network  the one commands run without a network on isn't there: whiff sandbox build\n")
			return 1
		}
		return 0
	default: // build
		if err := b.MakeNetwork(ctx); err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		if err := b.Build(ctx, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "whiff: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "built %s\n", sandbox.Image)
		return 0
	}
}
