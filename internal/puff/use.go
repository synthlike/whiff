package puff

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func isUse(f *Form) bool {
	return f.Kind == FormList && len(f.Items) > 0 && f.Items[0].Kind == FormSymbol && f.Items[0].Name == "use"
}

// ExpandUses returns forms with each (use "path") among them replaced by
// the forms of the file it names, whose own uses are expanded in turn. A
// used file holds only defs, tests and uses. A relative path starts from
// the directory of file, the file forms came from, or from dir if file is
// "" ("" is the working directory); in a used file, from that file's. A
// file that is a symlink resolves from its target's directory. Each file
// comes in once, however many use it; one that uses itself, by any way
// round, is an error at the use that closes the cycle. files are the
// files it read or tried to, even when it fails, so a host can watch
// them for changes.
func ExpandUses(forms []*Form, file, dir string) (out []*Form, files []string, err error) {
	x := &uses{done: map[string]bool{}}
	var stack []using
	if file != "" {
		dir = useDir(file)
		stack = []using{{realPath(file), file}}
	}
	out, err = x.expand(forms, dir, stack)
	return out, x.files, err
}

type uses struct {
	done  map[string]bool // by real path
	files []string
}

// using is a file whose uses are being expanded: its real path, and its
// path as named, for errors.
type using struct{ key, name string }

func (x *uses) expand(forms []*Form, dir string, stack []using) ([]*Form, error) {
	if !slices.ContainsFunc(forms, isUse) {
		return forms, nil
	}
	var out []*Form
	for _, f := range forms {
		if !isUse(f) {
			out = append(out, f)
			continue
		}
		var name String
		if len(f.Items) == 2 && f.Items[1].Kind == FormLiteral {
			name, _ = f.Items[1].Val.(String)
		}
		if name == "" {
			return nil, errorf(f.Pos, "use wants a file's path, as a string")
		}
		path := string(name)
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		key := realPath(path)
		if i := slices.IndexFunc(stack, func(u using) bool { return u.key == key }); i >= 0 {
			var cycle []string
			for _, u := range stack[i:] {
				cycle = append(cycle, u.name)
			}
			cycle = append(cycle, path)
			return nil, errorf(f.Pos, "a cycle of uses: %s", strings.Join(cycle, " uses "))
		}
		if x.done[key] {
			continue
		}
		x.done[key] = true
		x.files = append(x.files, key)
		src, err := os.ReadFile(path)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, errorf(f.Pos, "can't use %s: %v", path, err)
		}
		used, err := Read(path, string(src))
		if err != nil {
			return nil, err
		}
		for _, u := range used {
			if !isDef(u) && !isTest(u) && !isUse(u) {
				return nil, errorf(u.Pos, "a used file holds only defs, tests and uses")
			}
		}
		used, err = x.expand(used, useDir(path), append(stack, using{key, path}))
		if err != nil {
			return nil, err
		}
		out = append(out, used...)
	}
	return out, nil
}

// useDir is the directory a file's uses start from: its own, or its
// target's if it is a symlink, as a service in ~/.whiff/services often is.
func useDir(file string) string {
	if fi, err := os.Lstat(file); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := filepath.EvalSymlinks(file); err == nil {
			return filepath.Dir(target)
		}
	}
	return filepath.Dir(file)
}

// realPath is a file's absolute path, symlinks resolved where they can be,
// so two ways to name it are one file.
func realPath(file string) string {
	if p, err := filepath.EvalSymlinks(file); err == nil {
		file = p
	}
	if p, err := filepath.Abs(file); err == nil {
		file = p
	}
	return file
}
