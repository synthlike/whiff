package puff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Getenv is the variable name in environ, KEY=value pairs as os.Environ
// gives them, or the process's environment if environ is nil.
func Getenv(environ []string, name string) (string, bool) {
	if environ == nil {
		return os.LookupEnv(name)
	}
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Home is the home directory in environ: its HOME, or the process's.
func Home(environ []string) (string, error) {
	if environ != nil {
		if h, ok := Getenv(environ, "HOME"); ok && h != "" {
			return h, nil
		}
	}
	return os.UserHomeDir()
}

// LookPath is file as exec.LookPath finds it, on environ's PATH; on the
// process's, by exec.LookPath, if environ is nil.
func LookPath(environ []string, file string) (string, error) {
	if environ == nil || strings.Contains(file, "/") {
		return exec.LookPath(file)
	}
	path, _ := Getenv(environ, "PATH")
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}

// Command is exec.CommandContext for argv in environ: argv[0] found on
// its PATH and the process given it, or the process's own if environ is
// nil. A whiff test's daemon has an environment of its own this way, so
// tests with fake programs on their PATH run side by side.
func Command(ctx context.Context, environ []string, argv []string) *exec.Cmd {
	name := argv[0]
	if environ != nil {
		if p, err := LookPath(environ, name); err == nil {
			name = p
		}
	}
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	cmd.Args[0] = argv[0]
	if environ != nil {
		cmd.Env = environ
	}
	return cmd
}
