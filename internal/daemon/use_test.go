package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A used file's defs are the service's own: its handlers' marks see
// through them, and its tests run with the service's.
func TestUse(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	services := filepath.Join(dir, "services")
	writeServices(t, dir, map[string]string{
		"u": `(service u
  (use "lib/files.puff")
  (on (writes path) "Saves through a used def." (save path))
  (on (reads path) "Peeks through a used def." (peek path)))`,
	})
	if err := os.MkdirAll(filepath.Join(services, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	lib := `(def (save path) (file/write path "x"))
(def (peek path) (file/read-text path))
(test "peek reads" (= (peek "/nonexistent") (peek "/nonexistent")))`
	if err := os.WriteFile(filepath.Join(services, "lib", "files.puff"), []byte(lib), 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, dir)
	c := dial(t, dir)

	got, err := c.Eval("(address/describe :u)", "")
	if err != nil || got.Error != "" {
		t.Fatalf("describe :u: %v %s", err, got.Error)
	}
	for _, want := range []string{`:fn :reads :form :on :mark :read`, `:fn :writes :form :on :mark :write`} {
		if !strings.Contains(got.Value, want) {
			t.Errorf("describe :u = %s, want it to hold %s", got.Value, want)
		}
	}

	// whiff test, from the services directory: a relative file's uses
	// start from there.
	src, err := os.ReadFile(filepath.Join(services, "u.puff"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Test("u.puff", string(src), services)
	if err != nil || resp.Error != "" || !strings.Contains(resp.Value, `:test "peek reads"`) || !strings.Contains(resp.Value, `:message ""`) {
		t.Errorf("test u.puff = %+v %v, want lib's test, passed", resp, err)
	}

	agent := dial(t, dir)
	agent.Agent = true
	resp, err = agent.Eval(`(use "lib/files.puff") (peek "x")`, services)
	if want := "eval:1:1: an agent's eval can't use files; a service can"; err != nil || resp.Error != want {
		t.Errorf("an agent's use = %+v %v, want %s", resp, err, want)
	}
}

// A service reloads when a file it uses changes, even one that broke its
// first load.
func TestReloadUses(t *testing.T) {
	set(t, &pollInterval, 10*time.Millisecond)

	dir := tempDir(t)
	services := filepath.Join(dir, "services")
	writeServices(t, dir, map[string]string{
		"greet": `(service greet (use "lib/words.puff") (on (hello name) "Greets name." (ok (str word ", " name))))`,
	})
	lib := filepath.Join(services, "lib", "words.puff")
	if err := os.MkdirAll(filepath.Dir(lib), 0o700); err != nil {
		t.Fatal(err)
	}
	version := 0
	write := func(src string) {
		t.Helper()
		if err := os.WriteFile(lib, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		version++
		at := time.Now().Add(time.Duration(version) * time.Second)
		if err := os.Chtimes(lib, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(`(def word "hello"`)
	start(t, dir)
	c := dial(t, dir)
	becomes := func(src, want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			resp, err := c.Eval(src, "")
			if err != nil {
				t.Fatal(err)
			}
			if got = resp.Value + resp.Error; got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s = %s, want %s", src, got, want)
			}
		}
	}
	hello := `(match (call :greet/hello "ann") [(err {:kind k}) k] [r r])`
	becomes(hello, ":no-address")
	write(`(def word "hello")`)
	becomes(hello, `(ok "hello, ann")`)
	write(`(def word "hi")`)
	becomes(hello, `(ok "hi, ann")`)
}
