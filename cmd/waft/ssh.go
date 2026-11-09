package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A runner runs a shell script on a host, as root or, with as, as that user,
// with input, if any, on its stdin. A script's words are visible to every
// user on the host, so a secret goes in input.
type runner interface {
	run(ctx context.Context, as, script string, input []byte) (result, error)
}

// result is how a script went: a nonzero exit is a result, not an error.
type result struct {
	out, stderr string
	code        int
}

// sshRunner runs scripts on dest, an admin account with passwordless sudo,
// over one connection that a control master keeps open.
type sshRunner struct {
	dest, dir string
}

// dial opens dest's runner and returns it with its close.
func dial(dest string) (runner, func(), error) {
	// In /tmp, not $TMPDIR: macOS's is too long for a socket path.
	dir, err := os.MkdirTemp("/tmp", "waft-")
	if err != nil {
		return nil, nil, err
	}
	s := &sshRunner{dest: dest, dir: dir}
	return s, func() {
		exec.Command("ssh", append(s.opts(), "-O", "exit", dest)...).Run()
		os.RemoveAll(dir)
	}, nil
}

func (s *sshRunner) opts() []string {
	return []string{"-o", "BatchMode=yes", "-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(s.dir, "%C"), "-o", "ControlPersist=60"}
}

func (s *sshRunner) run(ctx context.Context, as, script string, input []byte) (result, error) {
	cmd := exec.CommandContext(ctx, "ssh", append(s.opts(), s.dest, remote(as, script))...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), &stdout, &stderr
	err := cmd.Run()
	res := result{out: stdout.String(), stderr: strings.ReplaceAll(strings.TrimSpace(stderr.String()), "\n", "; ")}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() != 255:
		res.code = exit.ExitCode()
	case err != nil:
		return result{}, fmt.Errorf("ssh %s: %v: %s", s.dest, err, res.stderr)
	}
	return res, nil
}

// quote is s as one word for sh.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteAll is ss as words for sh.
func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return strings.Join(q, " ")
}

// shPath is a host path for sh, with a leading ~ as the running user's home.
func shPath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + quote(p[2:])
	}
	return quote(p)
}

// remote is the command that runs script on the host as as, root if empty,
// in its home: sudo keeps the admin's folder, which another user may not
// be able to read, and go, for one, won't start there.
func remote(as, script string) string {
	sudo := "sudo -n -H"
	if as != "" {
		sudo += " -u " + quote(as)
	}
	return sudo + " -- sh -c " + quote("cd \"$HOME\" || exit 1\n"+script)
}
