package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

var markServices = map[string]string{
	"files": `(service files
  (on (save path text) "Writes a file." (file/write path text))
  (on (peek path) "Reads a file." (file/read-text path))
  (on (status) :read "Runs make status, which only reads." (proc/run ["make" "status"]))
  (on (remember x) :write "Keeps x." (ok x)))`,
	"cmds": `(service cmds
  (on (status) "Runs git status." (proc/output ["git" "status" "--porcelain"]))
  (on (push) "Runs git push." (proc/run ["git" "push"]))
  (on (computed args) "Runs git with args." (proc/run (vector/concat ["git"] args)))
  (on (log) "Starts git log." (call :jobs/run ["git" "log" "--oneline"] {:dir "/"}))
  (on (build) "Starts go build." (call :jobs/run ["go" "build"])))`,
	"user": `(service user
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
	"count": `(service count :state 0
  (on (keep n x) :update "Keeps x." (ok [n x]))
  (on (cache n x) :read :update "Caches x, which only reads." (ok [n x]))
  (on (get n) "Reads the state." (ok n)))`,
}

func TestDescribe(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, markServices)
	start(t, dir)
	c := dial(t, dir)

	// marks is each function's name and mark from describe.
	marks := func(addr string) string {
		got, err := c.Eval("(address/describe :"+addr+")", "")
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
		{"count", `:cache :read
:get :read
:keep :write`},
		{"cmds", `:build :write
:computed :write
:log :read
:push :write
:status :read`},
	}
	for _, tt := range tests {
		if got := marks(tt.addr); got != tt.want {
			t.Errorf("marks of :%s:\n%s\nwant:\n%s", tt.addr, got, tt.want)
		}
	}

	got, err := c.Eval("(address/describe :files)", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{:doc "Reads a file." :fn :peek :form :on :mark :read :params [:path]}`; !strings.Contains(got.Value, want) {
		t.Errorf("describe :files = %s, want it to hold %s", got.Value, want)
	}
	got, err = c.Eval("(address/describe :nobody)", "")
	if want := `(err {:kind :no-address :message "no address :nobody"})`; err != nil || got.Value != want {
		t.Errorf("describe :nobody = %v %s, want %s", err, got.Value, want)
	}
}

func TestLs(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"files": markServices["files"],
		"greet": `(service greet
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
	files := filepath.Join(dir, "services", "files.puff")
	greet := filepath.Join(dir, "services", "greet.puff")
	want := `:files
  (peek path)       :read   Reads a file.
  (remember x)      :write  Keeps x.
  (save path text)  :write  Writes a file.
  (status)          :read   Runs make status, which only reads.
:greet
  (hello name)  :read   Greets name.
  (lost)        :write  Calls targets that don't exist.
:jobs
  (cancel id)       :write  Stop job id and everything it started: SIGTERM, then SIGKILL for what's left after 3s.
  (list)            :read   The running jobs, each {:id :cmd :cwd :from}.
  (output id)       :read   (ok {:stdout :stderr}), what running job id has written so far: the last 64KiB of each. (err {:kind :no-id}) once it isn't running: its result is in whiff wait.
  (process id)      :read   (some pid), the process group job id may still run in: while it runs, and after whiff died while it ran, while its result's :pid is there; else (none). Retry its write only once it's (none).
  (run cmd [opts])  :write  Run cmd, [program args...], here; pending now, {:exit :stdout :stderr} when done. Options: :timeout (1h), or :none for a server; :stdin, a string; :dir, where it runs; :as :text answers (ok stdout), or (err {:kind :exit}) unless it exits 0; :origin :agent, for a job whose output a model writes, answers as :agent's; :retry n runs it again, up to n times, under its id, if a timeout or whiff stopped it, or whiff died while it ran, and only if it is :read or :idempotent #t says running it twice is safe. :read for a read-only command, as whiff ls lists them.
:me
  (answer id [text])         :write  Reply to message id in your inbox (ok text), or (ok :done) without text or with "", which dismisses a report.
  ask (ask question [opts])  :read   Asks you question: pending now, your answer when you reply. opts, {:choices ["yes" "no"]}, offers answers to pick.
  (inbox)                    :read   (ok view): what waits in your inbox, oldest first: questions to answer and reports to dismiss.
  ask (tell text [opts])     :read   Tells you text, a report: it waits in your inbox until you reply, which dismisses it. opts, {:actions [{:label "Show the diff" :action [:address/fn args...]}]}, offers what to do about it.
:review
  (accept id)           :write  Deliver the held message id as it was sent, or run the code it holds; its result, which its sender gets too.
  (pending)             :read   (ok view): what waits for your review, as items to accept or reject, oldest first.
  (reject id [reason])  :write  Answer the held message id (err {:kind :rejected :reason reason}) without delivering it; reason, a string, tells its sender why.
  (trust id)            :write  Accept the held message id, an agent session's to a service, and trust the session for that service from now on, as whiff trust does.
read-only commands (internal/puff/commands.go): ` + strings.Join(puff.ReadOnlyCommands(), ", ") + `
override: ` + files + `:4:3: :files/status is marked :read, but reaches proc/run at 4:61
warning: ` + greet + `:5:15: :nobody/x doesn't exist
warning: ` + greet + `:5:32: :hello2 doesn't exist
`
	if got != want {
		t.Errorf("ls:\n%s\nwant:\n%s", got, want)
	}
}

func TestBackstop(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "f")
	writeServices(t, dir, map[string]string{
		"b": `(service b (on (x) "Reads." (ok 1)))`,
		"a": `(service a
  (on (twice) "Waits on hiss, then calls b."
    (do (call :hiss/hold) (call :b/x)))
  (on (status) :read "Runs a program, which only reads." (proc/output ["true"]))
  (on (via) "Counts on status's claim." (status))
  (on (save) "Writes." (file/write "` + file + `" "x")))`,
	})
	start(t, dir)
	c := dial(t, dir)
	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, "")
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}

	// hiss holds :a/twice until it is told to answer.
	hiss := dial(t, dir)
	if resp, err := hiss.Claim(":hiss", `(on (hold) "Holds the caller.")`); err != nil || resp.Value != "(ok :done)" {
		t.Fatalf("claim :hiss = %+v %v", resp, err)
	}
	held, answer := make(chan int64), make(chan bool)
	go func() {
		for d := range hiss.Deliveries() {
			held <- d.ID
			<-answer
			hiss.Answer(d.ID, "(ok :done)")
		}
	}()

	// An override, and what counts on it, run; so does a :write handler.
	for src, want := range map[string]string{
		`(call :a/status)`: `(ok "")`,
		`(call :a/via)`:    `(ok "")`,
		`(call :a/save)`:   `(ok :done)`,
	} {
		if got := eval(src); got != want {
			t.Errorf("%s = %s, want %s", src, got, want)
		}
	}

	// :a/twice is :read while :b/x is.
	go func() { <-held; answer <- true }()
	if got := eval(`(call :a/twice)`); got != "(ok 1)" {
		t.Fatalf("(call :a/twice) = %s", got)
	}

	// Once :b/x is :write, :a/twice faults on it, even mid-message.
	got := make(chan string)
	other := dial(t, dir) // a client waits for one request at a time
	go func() {
		resp, err := other.Eval(`(call :a/twice)`, "")
		if err != nil {
			resp.Value = err.Error()
		}
		got <- resp.Value
	}()
	<-held
	writeServices(t, dir, map[string]string{"b": `(service b (on (x) "Writes." (file/write "` + file + `" "y")))`})
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(eval(`(address/describe :b)`), ":mark :write"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(":b didn't reload")
		}
	}
	answer <- true
	if r, want := <-got, ":b/x is :write, and :a/twice is :read"; !strings.Contains(r, want) {
		t.Errorf("(call :a/twice) after the reload = %s, want it to hold %q", r, want)
	}
	if b, _ := os.ReadFile(file); string(b) != "x" {
		t.Errorf("file = %q, want :b/x not to have run", b)
	}
}
