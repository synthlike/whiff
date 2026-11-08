package huff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Local is the tools of a model that works on files: claude -p's Read,
// Write, Edit, Glob, Grep and Bash, by the same names and arguments, so a
// prompt written for one works for the other. They act only inside Dir:
// a path outside it, or a symlink that leads out of it, is refused. Bash
// runs only the commands Allow names, without a shell: a command exactly,
// as "go test ./...", or a prefix and what follows it, as "go vet:*";
// pipes, redirections, $ and ; are refused.
type Local struct {
	Dir     string
	Names   []string      // the tools it offers, of LocalTools
	Allow   []string      // the commands Bash may run
	Timeout time.Duration // what a command may take, 10m if 0

	Denied []string // the Bash commands it refused, as "go run ."
}

// LocalTools are the tools Local may offer, in order.
var LocalTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"}

const (
	readLines  = 2000    // lines Read gives if not told
	outputMax  = 30000   // characters of a command's output it keeps, its end
	matchesMax = 200     // lines Grep gives
	globMax    = 500     // paths Glob gives
	grepFile   = 1 << 20 // the largest file Grep reads
)

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

func object(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var localTools = map[string]Tool{
	"Read": {Name: "Read", Description: "Reads a file, its lines numbered, 2000 at most unless told; offset is the line to start at, from 1.",
		Params: object(map[string]any{"file_path": str("the file, absolute or from the working directory"),
			"offset": num("the line to start at, from 1"), "limit": num("how many lines")}, "file_path")},
	"Write": {Name: "Write", Description: "Writes content to a file, replacing it if it is there, making its directories.",
		Params: object(map[string]any{"file_path": str("the file, absolute or from the working directory"), "content": str("all of the file")}, "file_path", "content")},
	"Edit": {Name: "Edit", Description: "Replaces old_string in a file with new_string. old_string must be in it exactly once, unless replace_all; copy it exactly, indentation too.",
		Params: object(map[string]any{"file_path": str("the file"), "old_string": str("the text to replace"),
			"new_string": str("what replaces it"), "replace_all": flag("replace every one")}, "file_path", "old_string", "new_string")},
	"Glob": {Name: "Glob", Description: "The files whose paths match a pattern, as **/*.go or cmd/*/main.go, from the working directory or path.",
		Params: object(map[string]any{"pattern": str("the pattern; ** matches any directories"), "path": str("the directory to look in")}, "pattern")},
	"Grep": {Name: "Grep", Description: "The lines that match a regular expression (Go's syntax), as file:line: text, in a file or the files under a directory; glob limits which files, as *.go.",
		Params: object(map[string]any{"pattern": str("the regular expression"), "path": str("a file or directory, the working directory if none"),
			"glob": str("only files whose names match, as *.go")}, "pattern")},
	"Bash": {Name: "Bash", Description: "Runs a command in the working directory and gives its output, without a shell: no pipes, redirections or $. Only some commands are allowed.",
		Params: object(map[string]any{"command": str("the command, as go test ./...")}, "command")},
}

// Tools are the tools l offers.
func (l *Local) Tools() []Tool {
	var ts []Tool
	for _, n := range LocalTools {
		if slices.Contains(l.Names, n) {
			t := localTools[n]
			if n == "Bash" {
				t.Description += " These: " + strings.Join(l.Allow, ", ") + " (a name ending in :* allows what follows it)."
			}
			ts = append(ts, t)
		}
	}
	return ts
}

// Offers says whether l offers the tool named name.
func (l *Local) Offers(name string) bool { return slices.Contains(l.Names, name) }

// Call runs c, one of l's tools.
func (l *Local) Call(ctx context.Context, c Call) (string, bool) {
	if !l.Offers(c.Fn) {
		return fmt.Sprintf("there is no tool %s", c.Fn), true
	}
	var a struct {
		FilePath   string `json:"file_path"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Content    string `json:"content"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		Command    string `json:"command"`
	}
	if strings.TrimSpace(c.Args) != "" {
		if err := json.Unmarshal([]byte(c.Args), &a); err != nil {
			return fmt.Sprintf("its arguments aren't a JSON object of the right types: %v", err), true
		}
	}
	var out string
	var err error
	switch c.Fn {
	case "Read":
		out, err = l.read(a.FilePath, a.Offset, a.Limit)
	case "Write":
		out, err = l.write(a.FilePath, a.Content)
	case "Edit":
		out, err = l.edit(a.FilePath, a.OldString, a.NewString, a.ReplaceAll)
	case "Glob":
		out, err = l.glob(a.Pattern, a.Path)
	case "Grep":
		out, err = l.grep(a.Pattern, a.Path, a.Glob)
	case "Bash":
		return l.bash(ctx, a.Command)
	}
	if err != nil {
		return err.Error(), true
	}
	return out, false
}

// resolve is p, from Dir if relative, if it is inside Dir, following the
// symlinks of the part of it that is there.
func (l *Local) resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("name a file")
	}
	root, err := filepath.EvalSymlinks(l.Dir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(l.Dir, p)
	}
	p = filepath.Clean(p)
	// Follow the symlinks of the longest part of p that is there.
	there, rest := p, ""
	for {
		real, err := filepath.EvalSymlinks(there)
		if err == nil {
			p = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(there)
		if parent == there {
			break
		}
		rest = filepath.Join(filepath.Base(there), rest)
		there = parent
	}
	if r, err := filepath.Rel(root, p); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s, where you work", p, l.Dir)
	}
	return p, nil
}

func (l *Local) read(file string, offset, limit int) (string, error) {
	p, err := l.resolve(file)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "(the file is empty)", nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readLines
	}
	if offset > len(lines) {
		return "", fmt.Errorf("it has %d lines, none from %d", len(lines), offset)
	}
	end := min(len(lines), offset-1+limit)
	var sb strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&sb, "%6d\t%s", i+1, strings.TrimSuffix(lines[i], "\n")+"\n")
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "(%d more lines: read on from offset %d)\n", len(lines)-end, end+1)
	}
	return sb.String(), nil
}

func (l *Local) write(file, content string) (string, error) {
	p, err := l.resolve(file)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s, %d lines", l.rel(p), strings.Count(content, "\n")+boolInt(content != "" && !strings.HasSuffix(content, "\n"))), nil
}

func (l *Local) edit(file, old, new string, all bool) (string, error) {
	p, err := l.resolve(file)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(b)
	if old == "" {
		return "", errors.New("old_string is empty: to write a whole file, use Write")
	}
	if old == new {
		return "", errors.New("old_string and new_string are the same")
	}
	n := strings.Count(s, old)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string isn't in %s: read it again and copy the text exactly", l.rel(p))
	case n > 1 && !all:
		return "", fmt.Errorf("old_string is in %s %d times: give more of the text around it, or replace_all", l.rel(p), n)
	}
	if all {
		s = strings.ReplaceAll(s, old, new)
	} else {
		s = strings.Replace(s, old, new, 1)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s: %d replaced", l.rel(p), map[bool]int{true: n, false: 1}[all]), nil
}

// rel is p from Dir, as the model sees paths.
func (l *Local) rel(p string) string {
	root, _ := filepath.EvalSymlinks(l.Dir)
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// walk calls f with each regular file under dir, its path from dir with
// slashes, skipping .git.
func walk(dir string, f func(p, rel string) bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !f(p, filepath.ToSlash(rel)) {
			return filepath.SkipAll
		}
		return nil
	})
}

// globMatch says whether name, a slash path, matches pattern, where **
// matches any number of directories.
func globMatch(pattern, name string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchParts(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func (l *Local) glob(pattern, dir string) (string, error) {
	if pattern == "" {
		return "", errors.New("give a pattern, as **/*.go")
	}
	if dir == "" {
		dir = l.Dir
	}
	d, err := l.resolve(dir)
	if err != nil {
		return "", err
	}
	if _, err := path.Match(strings.ReplaceAll(pattern, "**", "x"), ""); err != nil {
		return "", fmt.Errorf("the pattern %q: %v", pattern, err)
	}
	var found []string
	more := false
	walk(d, func(p, rel string) bool {
		if globMatch(pattern, rel) {
			if len(found) == globMax {
				more = true
				return false
			}
			found = append(found, l.rel(p))
		}
		return true
	})
	if len(found) == 0 {
		return "no files match " + pattern, nil
	}
	slices.Sort(found)
	out := strings.Join(found, "\n")
	if more {
		out += fmt.Sprintf("\n(the first %d: narrow the pattern)", globMax)
	}
	return out, nil
}

func (l *Local) grep(pattern, where, glob string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("the pattern: %v", err)
	}
	if where == "" {
		where = l.Dir
	}
	d, err := l.resolve(where)
	if err != nil {
		return "", err
	}
	var out []string
	more := false
	walk(d, func(p, rel string) bool {
		if glob != "" {
			if ok, _ := path.Match(glob, path.Base(rel)); !ok && !globMatch(glob, rel) {
				return true
			}
		}
		if info, err := os.Stat(p); err != nil || info.Size() > grepFile {
			return true
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			return true
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				if len(out) == matchesMax {
					more = true
					return false
				}
				out = append(out, fmt.Sprintf("%s:%d: %s", l.rel(p), i+1, line))
			}
		}
		return true
	})
	if len(out) == 0 {
		return "no lines match " + pattern, nil
	}
	s := strings.Join(out, "\n")
	if more {
		s += fmt.Sprintf("\n(the first %d: narrow the pattern or the path)", matchesMax)
	}
	return s, nil
}

// words is command split as a shell would split words, with quotes, or
// an error if it uses what needs a shell.
func words(command string) ([]string, error) {
	var ws []string
	var w strings.Builder
	inWord := false
	var quote rune
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				w.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				ws = append(ws, w.String())
				w.Reset()
				inWord = false
			}
		case strings.ContainsRune("|&;<>`$()\n\\", r):
			return nil, fmt.Errorf("%q needs a shell, which Bash here hasn't: run one command, without pipes, redirections, $ or ;", string(r))
		default:
			w.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("a quote isn't closed")
	}
	if inWord {
		ws = append(ws, w.String())
	}
	if len(ws) == 0 {
		return nil, errors.New("give a command")
	}
	return ws, nil
}

// allowed says whether ws is a command allow lets run.
func allowed(allow []string, ws []string) bool {
	line := strings.Join(ws, " ")
	for _, a := range allow {
		if prefix, ok := strings.CutSuffix(a, ":*"); ok {
			if line == prefix || strings.HasPrefix(line, prefix+" ") {
				return true
			}
		} else if line == strings.Join(strings.Fields(a), " ") {
			return true
		}
	}
	return false
}

func (l *Local) bash(ctx context.Context, command string) (string, bool) {
	ws, err := words(command)
	if err != nil {
		return err.Error(), true
	}
	if !allowed(l.Allow, ws) {
		l.Denied = append(l.Denied, strings.Join(ws, " "))
		if len(l.Allow) == 0 {
			return "no command is allowed here", true
		}
		return fmt.Sprintf("that command isn't allowed: only %s (a name ending in :* allows what follows it)", strings.Join(l.Allow, ", ")), true
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ws[0], ws[1:]...)
	cmd.Dir = l.Dir
	out, err := cmd.CombinedOutput()
	s := string(out)
	if len(s) > outputMax {
		s = fmt.Sprintf("(its first %d characters are cut)\n", len(s)-outputMax) + s[len(s)-outputMax:]
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return s + fmt.Sprintf("\n(stopped after %v)", timeout), true
	case errors.As(err, &exit):
		return s + fmt.Sprintf("\n(exit %d)", exit.ExitCode()), true
	case err != nil:
		return err.Error(), true
	}
	if s == "" {
		s = "(no output)"
	}
	return s, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Toolboxes are several toolboxes as one: a call goes to the first that
// offers its tool.
type Toolboxes []Toolbox

func (bs Toolboxes) Tools() []Tool {
	var ts []Tool
	for _, b := range bs {
		ts = append(ts, b.Tools()...)
	}
	return ts
}

func (bs Toolboxes) Call(ctx context.Context, c Call) (string, bool) {
	for _, b := range bs {
		for _, t := range b.Tools() {
			if t.Name == c.Fn {
				return b.Call(ctx, c)
			}
		}
	}
	return fmt.Sprintf("there is no tool %s", c.Fn), true
}
