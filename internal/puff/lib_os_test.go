package puff

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIOLib(t *testing.T) {
	env := NewEnv()
	var out strings.Builder
	env.Stdout = &out
	if got := load(env, `(io/println "a" 1 [2 "b"])`); got != "(ok :done)" {
		t.Errorf("io/println = %s", got)
	}
	if got, want := out.String(), "a 1 [2 \"b\"]\n"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
}

func TestEnvLib(t *testing.T) {
	t.Setenv("PUFF_TEST", "1")
	testLib(t, []struct{ src, want string }{
		{`[(env/get "PUFF_TEST") (env/get "PUFF_TEST_UNSET")]`, `[(some "1") (none)]`},
		{`(path/join "a" "b" "../c")`, `"a/c"`},
		{`[(path/base "a/c.txt") (path/dir "a/c.txt") (path/ext "a/c.txt") (path/ext "a/c")]`, `["c.txt" "a" ".txt" ""]`},
	})
	env := NewEnv()
	env.Dir = "/tmp/w"
	if got, want := load(env, `[(os/cwd) (path/abs "x")]`), `["/tmp/w" "/tmp/w/x"]`; got != want {
		t.Errorf("with Dir: %s, want %s", got, want)
	}
	wd, _ := os.Getwd()
	if got, want := load(NewEnv(), `(os/cwd)`), fmt.Sprintf("%q", wd); got != want {
		t.Errorf("os/cwd = %s, want %s", got, want)
	}
}

func TestFileLib(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	testLib(t, []struct{ src, want string }{
		{fmt.Sprintf(`(file/write %q "hi\n")`, a), "(ok :done)"},
		{fmt.Sprintf(`(file/read-text %q)`, a), `(ok "hi\n")`},
		{fmt.Sprintf(`(file/write-atomic %q "new")`, a), "(ok :done)"},
		{fmt.Sprintf(`(file/read-text %q {})`, a), `(ok "new")`},
		{fmt.Sprintf(`[(file/exists? %q) (file/exists? %q)]`, a, filepath.Join(dir, "b.txt")), "[#t #f]"},
		{fmt.Sprintf(`(file/list %q)`, dir), `(ok ["a.txt"])`},
	})
	if got := load(NewEnv(), fmt.Sprintf(`(file/read-text %q)`, filepath.Join(dir, "none.txt"))); !strings.HasPrefix(got, "(err {:kind :not-found") {
		t.Errorf("reading a missing file: %s", got)
	}
	if got := load(NewEnv(), fmt.Sprintf(`(file/write %q "x")`, filepath.Join(dir, "no", "dir.txt"))); !strings.HasPrefix(got, "(err {:kind :not-found") {
		t.Errorf("writing into a missing dir: %s", got)
	}

	env := NewEnv()
	env.Dir = dir
	if got, want := load(env, `(file/read-text "a.txt")`), `(ok "new")`; got != want {
		t.Errorf("relative to Dir: %s, want %s", got, want)
	}
}

func TestProcLib(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	testLib(t, []struct{ src, want string }{
		{`(proc/run ["echo" "hi"])`, `(ok {:exit 0 :stderr "" :stdout "hi\n"})`},
		{`(proc/run ["cat"] {:stdin "x"})`, `(ok {:exit 0 :stderr "" :stdout "x"})`},
		{`(proc/run ["sh" "-c" "echo e >&2; exit 3"])`, `(ok {:exit 3 :stderr "e\n" :stdout ""})`},
		{fmt.Sprintf(`(proc/run ["ls"] {:dir %q})`, dir), `(ok {:exit 0 :stderr "" :stdout "a.txt\n"})`},

		{`(proc/run [])`, "1:1: proc/run: want a command and its arguments, got []"},
		{`(proc/run ["ls"] {:timeout 5})`, "1:1: proc/run: want a positive :timeout duration, got 5"},
	})
	if got := load(NewEnv(), `(proc/run ["no-such-command-puff"])`); !strings.HasPrefix(got, "(err {:kind :not-found") {
		t.Errorf("a missing command: %s", got)
	}

	// A timeout kills the whole process group: sh and the sleep it started.
	start := time.Now()
	got := load(NewEnv(), `(proc/run ["sh" "-c" "sleep 5; echo done"] {:timeout 100ms})`)
	if want := `(err {:kind :timeout :message "sh took longer than 100ms"})`; got != want {
		t.Errorf("timeout: %s, want %s", got, want)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %s", d)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, want := loadCtx(ctx, NewEnv(), `(proc/run ["sleep" "5"])`), "1:1: proc/run: cancelled"; got != want {
		t.Errorf("cancelled: %s, want %s", got, want)
	}
}

func TestTimeLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`(time/from-unix-ms 0)`, `1970-01-01T00:00:00Z`},
		{`(time/from-string "2026-10-27T20:25:00+02:00")`, `(ok 2026-10-27T18:25:00Z)`},
		{`(time/to-unix-ms (time/from-unix-ms 1500))`, `1500`},
		{`[(time/to-string (time/from-unix-ms 0)) (time/to-string 1970-01-01T00:00:01.000000005Z)]`, `["1970-01-01T00:00:00Z" "1970-01-01T00:00:01.000000005Z"]`},
		{`(-> (time/from-unix-ms 0) (time/to-string {:zone "Europe/Warsaw"}))`, `"1970-01-01T01:00:00+01:00"`},
		{`[(< (time/from-unix-ms 0) (time/now)) (> (time/since (time/from-unix-ms 0)) 0s)]`, `[#t #t]`},
		{`(time/from-string "3000-01-01T00:00:00Z")`, `(err {:kind :parse :message "3000-01-01T00:00:00Z is out of range: times are from 1678 to 2262"})`},

		{`(time/to-unix-ms 5)`, "1:1: time/to-unix-ms: want a time, got 5"},
		{`(match (value/from-string "(time \"1970-01-01T00:00:00.000Z\")") [(ok v) (time/to-unix-ms v)])`, `1:74: time/to-unix-ms: want a time, got (time "1970-01-01T00:00:00.000Z")`},
		{`(time/from-unix-ms 9223372036854775807)`, "1:1: time/from-unix-ms: 9223372036854775807 ms is out of range: times are from 1678 to 2262"},
		{`(time/to-string (time/now) {:zone "Mars/Base"})`, "1:1: time/to-string: unknown time zone Mars/Base"},
	})
	if got := load(NewEnv(), `(time/from-string "yesterday")`); !strings.HasPrefix(got, "(err {:kind :parse") {
		t.Errorf("a bad time: %s", got)
	}
}

func TestEffectMarks(t *testing.T) {
	for name, want := range map[string]Mark{
		"file/write": MarkWrite, "file/write-atomic": MarkWrite, "proc/run": MarkWrite,
		"file/read-text": MarkRead, "io/println": MarkRead, "time/now": MarkRead,
	} {
		if got := markOf(t, NewEnv(), name); got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
}
