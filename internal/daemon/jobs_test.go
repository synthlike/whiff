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
  (on (via) "Gets a job from hander." (call :hander/cat "handed on"))
  (on (whiff/done id r) "Writes the job's output."
    (match r [(ok {:stdout out}) (file/write "` + got + `" out)])))`,
		"watch": `(srv watch
  (on :jobs/done (v) (ok v)))`,
		"hander": `(srv hander
  (on (cat text) "Hands its caller a job." (hand-on (call :jobs/run ["cat"] {:stdin text})))
  (on (late) "Hands on a job that is done already, twice."
    (let [r (call :jobs/run ["echo" "late"])]
      (do (loop []
            (match (call :jobs/list)
              [(ok []) (ok :done)]
              [_ (do (proc/run ["sleep" "0.01"]) (recur))]))
          (match (hand-on r) [(err {:kind :pending}) (hand-on r)]))))
  (on (whiff/done id r) "Writes the answers it got." (file/write "` + filepath.Join(dir, "hander-got") + `" (value/to-string r)))
  (on (pass x) "Hands on an ask." (hand-on (call :box/q x)))
  (on (bogus) "Hands on what isn't its own." (hand-on (err {:kind :pending :id 999})))
  (on (plain) "Hands on a value." (hand-on (ok 1))))`,
		"box":  `(srv box (ask (q x) "Waits."))`,
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

	id = run(`(call :jobs/run ["cat"] {:stdin "piped"})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 0 :stderr "" :stdout "piped"})`)
	id = run(`(call :jobs/run ["pwd"] {:dir "/" :as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok "/\n")`)
	id = run(`(call :jobs/run ["cat"] {:stdin "as text" :as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok "as text")`)
	id = run(`(call :jobs/run ["sh" "-c" "echo no >&2; exit 2"] {:as :text})`)
	inHistory(`done  user   :jobs -> client ` + id + ` (err {:exit 2 :kind :exit :message "job ` + id + ` exited 2" :stderr "no\n"})`)

	// hand-on hands the caller a job or an ask: to a client, to a service's
	// whiff/done, and when the answer came before the hand-on.
	id = run(`(call :hander/cat "to you")`)
	inHistory(`done  user   :jobs -> client ` + id + ` (ok {:exit 0 :stderr "" :stdout "to you"})`)
	run(`(call :builder/via)`)
	eventually(t, "the job hander handed on", func() string { b, _ := os.ReadFile(got); return string(b) }, "handed on")
	// The answer came first, so both hander and its caller get it; the
	// second hand-on finds nothing left to hand on.
	resp, err := c.Eval(`(call :hander/late)`, dir)
	if err != nil || !strings.HasPrefix(resp.Value, `(err {:kind :no-id :message "no job or ask `) {
		t.Errorf("handing on twice = %s %v", resp.Value, err)
	}
	inHistory(` (ok {:exit 0 :stderr "" :stdout "late\n"})`)
	h, _ := c.History()
	if !strings.Contains(h, `  done  user   :jobs -> client `) || !strings.Contains(h, `:jobs -> :hander/whiff/done `) {
		t.Errorf("want the late answer to go to both the client and hander:\n%s", h)
	}
	id = run(`(call :hander/pass 5)`)
	if _, err := c.Message("reply", "", "["+id+" (ok 6)]", ""); err != nil {
		t.Fatal(err)
	}
	inHistory(`done  user   client -> client ` + id + ` (ok 6)`)
	for src, want := range map[string]string{
		`(call :hander/bogus)`: `(err {:kind :no-id :message "no job or ask 999 of :hander's to hand on"})`,
		`(call :hander/plain)`: `(ok 1)`,
	} {
		if resp, err := c.Eval(src, dir); err != nil || resp.Value != want {
			t.Errorf("%s = %s %s %v, want %s", src, resp.Value, resp.Error, err, want)
		}
	}
	// A send has nobody waiting for its answer, so there is nobody to hand on to.
	c.Eval(`(send :hander/cat "x")`, dir)
	inHistory(`send  user   client -> :hander/cat "x"  (err {:kind :no-id :message "only a call's handler can hand on: nobody waits for this message's answer"})`)

	id = run(`(call :jobs/run ["sleep" "10"])`)
	resp, err = c.Eval(`(call :jobs/list)`, dir)
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
		{`(call :jobs/run ["true"] {:wait 1s})`, `(err {:kind :bad-call :message ":jobs/run: want {:timeout d :stdin s :dir path :as :text}, a positive duration and strings, got {:wait 1s}"})`},
		{`(call :jobs/run ["true"] {:as :json})`, `(err {:kind :bad-call :message ":jobs/run: want {:timeout d :stdin s :dir path :as :text}, a positive duration and strings, got {:as :json}"})`},
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

	if resp, err := c.Eval(`(hand-on 1)`, dir); err != nil || !resp.Fault || resp.Error != "eval:1:1: hand-on: only a service's handler can hand on" {
		t.Errorf("hand-on from a client = %+v %v", resp, err)
	}

	// A service file can't take :jobs's address.
	inHistory(`jobs.puff:1:1: :jobs is whiff's own address; rename the file`)
}
