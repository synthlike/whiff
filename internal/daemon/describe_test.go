package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

var markServices = map[string]string{
	"files": `(srv files
  (on (save path text) "Writes a file." (file/write path text))
  (on (peek path) "Reads a file." (file/read-text path))
  (on (status) :read "Runs git status, which only reads." (proc/run ["git" "status"]))
  (on (remember x) :write "Keeps x." (ok x)))`,
	"user": `(srv user
  (def (writer path) (call :files/save path "x"))
  (on (via-helper path) "Saves through a helper." (writer path))
  (on (reader path) "Calls a function that reads." (call :files/peek path))
  (on (overridden) "Calls one marked :read." (call :files/status))
  (on (computed t) "Calls a computed target." (call t))
  (on (missing) "Calls a target that doesn't exist." (call :nobody/x))
  (on (holds) "Holds call as a value." (let [f call] (ok 1)))
  (on (loop-a) "Calls b, which calls back." (call :loop-b))
  (on (loop-b) "Calls a." (call :loop-a))
  (on (sends) "Sends to a reader." (send :files/peek "x"))
  (on (later) "Sends to a writer, later." (send-after 1s :files/save "x" "y")))`,
}

func TestDescribe(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, markServices)
	start(t, dir)
	c := dial(t, dir)

	// marks is each function's name and mark from describe.
	marks := func(addr string) string {
		got, err := c.Eval("(describe :"+addr+")", "")
		if err != nil || got.Error != "" {
			t.Fatalf("describe %s: %v %s", addr, err, got.Error)
		}
		v, _ := puff.ReadValue(got.Value)
		fns := v.(puff.Tag).At(0).(puff.Vector)
		var lines []string
		for i := range fns.Len() {
			m := fns.At(i).(puff.Map)
			fn, _ := m.Get(puff.Keyword("fn"))
			mark, _ := m.Get(puff.Keyword("mark"))
			lines = append(lines, puff.Print(fn)+" "+puff.Print(mark))
		}
		return strings.Join(lines, "\n")
	}
	tests := []struct{ addr, want string }{
		{"files", `:peek :read
:remember :write
:save :write
:status :read`},
		{"user", `:computed :write
:holds :write
:later :write
:loop-a :read
:loop-b :read
:missing :write
:overridden :read
:reader :read
:sends :read
:via-helper :write`},
	}
	for _, tt := range tests {
		if got := marks(tt.addr); got != tt.want {
			t.Errorf("marks of :%s:\n%s\nwant:\n%s", tt.addr, got, tt.want)
		}
	}

	got, err := c.Eval("(describe :files)", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{:doc "Reads a file." :fn :peek :form :on :mark :read :params [:path]}`; !strings.Contains(got.Value, want) {
		t.Errorf("describe :files = %s, want it to hold %s", got.Value, want)
	}
	got, err = c.Eval("(describe :nobody)", "")
	if want := `(err {:kind :no-address :message "no address :nobody"})`; err != nil || got.Value != want {
		t.Errorf("describe :nobody = %v %s, want %s", err, got.Value, want)
	}
}

func TestLs(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"files": markServices["files"],
		"greet": `(srv greet
  (on (hello name) "Greets name."
    (ok (string/concat "hello, " name)))
  (on (lost) "Calls targets that don't exist."
    (do (send :nobody/x) (call :hello2))))`,
	})
	start(t, dir)
	c := dial(t, dir)
	got, err := c.Ls()
	if err != nil {
		t.Fatal(err)
	}
	// Columns line up within each address.
	greet := filepath.Join(dir, "services", "greet.puff")
	want := `:files
  (peek path)       :read   Reads a file.
  (remember x)      :write  Keeps x.
  (save path text)  :write  Writes a file.
  (status)          :read   Runs git status, which only reads.
:greet
  (hello name)  :read   Greets name.
  (lost)        :write  Calls targets that don't exist.
:jobs
  (cancel id)       :write  Kill job id and everything it started.
  (list)            :read   The running jobs, each {:id :cmd :cwd :from}.
  (run cmd [opts])  :write  Run cmd, [program args...], here; pending now, {:exit :stdout :stderr} when done. Options: :timeout (1h); :stdin, a string; :dir, where it runs; :as :text answers (ok stdout), or (err {:kind :exit}) unless it exits 0.
:me
  ask (ask question)  :read  Asks you question: pending now, your answer when you reply.
  ask (tell text)     :read  Tells you text, a report: it waits in your inbox until you reply, which dismisses it.
warning: ` + greet + `:5:15: :nobody/x doesn't exist
warning: ` + greet + `:5:32: :hello2 doesn't exist
`
	if got != want {
		t.Errorf("ls:\n%s\nwant:\n%s", got, want)
	}
}
