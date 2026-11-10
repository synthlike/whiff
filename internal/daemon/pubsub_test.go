package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublish(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	logged, mirrored := filepath.Join(dir, "logged"), filepath.Join(dir, "mirrored")
	writeServices(t, dir, map[string]string{
		"log": `(service log
  (on :note/added (v) (file/write "` + logged + `" v)))`,
		"mirror": `(service mirror
  (on :note/added (v) (file/write "` + mirrored + `" v))
  (on (shout v) "Publishes v." (publish :note/added v)))`,
		"bad": `(service bad
  (on :note/added (v) (+ 1 1.0)))`,
		"quiet": `(service quiet
  (on :ping/x (v) (ok v))
  (on (ping) "Publishes to a reader." (publish :ping/x 1)))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// holds waits until both files hold want.
	holds := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			a, _ := os.ReadFile(logged)
			b, _ := os.ReadFile(mirrored)
			if string(a) == want && string(b) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("files hold %q and %q, want %q", a, b, want)
			}
		}
	}
	for _, tt := range []struct{ src, want string }{
		{`(publish :note/added "a")`, "a"},
		{`(call :mirror/shout "b")`, "b"},
		{`(publish :nobody/listens 1)`, ""},
	} {
		got, err := c.Eval(tt.src, "")
		if err != nil || got.Value != "(ok :done)" {
			t.Fatalf("%s = %v %+v, want (ok :done)", tt.src, err, got)
		}
		if tt.want != "" {
			holds(tt.want)
		}
	}
	got, err := c.Message("pub", "note/added", `["c"]`, "")
	if err != nil || got.Value != "(ok :done)" {
		t.Fatalf("pub = %v %+v, want (ok :done)", err, got)
	}
	holds("c")
	if got, _ := c.Eval(`(publish "note/added" 1)`, ""); !got.Fault {
		t.Errorf(`publish "note/added" = %+v, want a fault`, got)
	}

	// A subscriber records its entry just after it has written its file.
	wants := []string{
		`  pub   user   :mirror -> :log on :note/added "b"  (ok :done)  `,
		`  pub   user   client -> :mirror on :note/added "c"  (ok :done)  `,
		`  pub   user   client -> :bad on :note/added "a"  (err {:kind :fault :message "` +
			filepath.Join(dir, "services", "bad.puff") + `:2:23: +: can't add int and float"})  `,
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h, err := historyOf(c)
		if err != nil {
			t.Fatal(err)
		}
		missing := ""
		for _, want := range wants {
			if !strings.Contains(h, want) {
				missing = want
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history:\n%s\nwant a line with %s", h, missing)
		}
	}

	// A publish counts as its subscribers' marks.
	ls, err := c.Ls()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  (shout v)           :write  Publishes v.\n  on :note/added (v)  :write\n",
		"  (ping)          :read  Publishes to a reader.\n  on :ping/x (v)  :read\n",
	} {
		if !strings.Contains(ls, want) {
			t.Errorf("ls:\n%s\nwant it to hold:\n%s", ls, want)
		}
	}
}
