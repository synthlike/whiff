package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobs(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	if err := os.WriteFile(filepath.Join(dir, "here"), []byte("in dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeServices(t, dir, map[string]string{
		"builder": `(srv builder
  (on (build) "Runs a job in the caller's directory." (call :jobs/run ["cat" "here"]))
  (on (whiff/done id r) "Writes the job's output."
    (match r [(ok {:stdout out}) (file/write "` + got + `" out)])))`,
		"watch": `(srv watch
  (on :jobs/done (v) (ok v)))`,
		"jobs": `(srv jobs)`,
	})
	start(t, dir)
	c := dial(t, dir)

	// run evaluates src, a call that should be pending, and returns its id.
	run := func(src string) string {
		t.Helper()
		resp, err := c.Eval(`(match `+src+` [(err {:kind :pending :id id}) id])`, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	inHistory := func(want string) {
		t.Helper()
		eventually(t, want, func() string {
			h, err := c.History()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(h, want) {
				return want
			}
			return "not in history"
		}, want)
	}

	id := run(`(call :jobs/run ["sh" "-c" "echo hi; echo oops >&2; exit 3"])`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 3 :stderr "oops\n" :stdout "hi\n"})`)
	inHistory(`pub   user   :jobs -> :watch on :jobs/done {:cmd ["sh" "-c" "echo hi; echo oops >&2; exit 3"] :cwd "` + dir + `" :id ` + id)

	run(`(call :builder/build)`)
	eventually(t, "builder's answer", func() string { b, _ := os.ReadFile(got); return string(b) }, "in dir")

	id = run(`(call :jobs/run ["sleep" "10"])`)
	resp, err := c.Eval(`(call :jobs/list)`, dir)
	if want := `(ok [{:cmd ["sleep" "10"] :cwd "` + dir + `" :id ` + id + `}])`; err != nil || resp.Value != want {
		t.Errorf("list = %s %v, want %s", resp.Value, err, want)
	}
	resp, err = c.Eval(`(call :jobs/cancel `+id+`)`, dir)
	if err != nil || resp.Value != "(ok :done)" {
		t.Errorf("cancel = %s %v", resp.Value, err)
	}
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:kind :cancelled`)

	id = run(`(call :jobs/run ["sleep" "10"] {:timeout 50ms})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:kind :timeout :message "job ` + id + ` took longer than 50ms"})`)

	tests := []struct{ src, want string }{
		{`(call :jobs/run [])`, `(err {:kind :bad-call :message ":jobs/run: want a command like [\"go\" \"test\"], got []"})`},
		{`(call :jobs/run ["true"] {:wait 1s})`, `(err {:kind :bad-call :message ":jobs/run: want {:timeout d}, a positive duration, got {:wait 1s}"})`},
		{`(call :jobs/run)`, `(err {:kind :bad-call :message ":jobs/run takes 1 to 2 arguments, got 0"})`},
		{`(match (call :jobs/run ["no-such-program-whiff"]) [(err {:kind k}) k])`, `:start`},
		{`(call :jobs/cancel 999)`, `(err {:kind :no-id :message "no job 999 is running"})`},
	}
	for _, tt := range tests {
		resp, err := c.Eval(tt.src, dir)
		if err != nil || resp.Value != tt.want {
			t.Errorf("%s = %s %s %v, want %s", tt.src, resp.Value, resp.Error, err, tt.want)
		}
	}

	// A service file can't take :jobs's address.
	inHistory(`jobs.puff:1:1: :jobs is whiff's own service; rename the file`)
}
