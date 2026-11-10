package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/puff"
)

// writeServices writes name.puff files into dir/services.
func writeServices(t testing.TB, dir string, files map[string]string) {
	services := filepath.Join(dir, "services")
	if err := os.MkdirAll(services, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(services, name+".puff"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCall(t *testing.T) {
	set(t, &defaultCallTimeout, 200*time.Millisecond)

	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"greet": `(service greet
  (def (greeting name) (string/concat "hello, " name))
  (on (hello name) "Greets name."
    (ok (greeting name)))
  (on (both a b) "Greets a and b, calling hello by name."
    (ok [(try (hello a)) (try (hello b))]))
  (on (refuse) "Fails as a value."
    (err {:kind :nope}))
  (on (boom) "Faults."
    (+ 1 1.0))
  (on (plain) "Returns a value that isn't a Result."
    5)
  (on (leak) "Replies with a function."
    (ok greeting))
  (on (cwd) "The caller's directory."
    (ok (os/cwd))))`,
		"relay": `(service relay
  (on (hello name) "Asks greet."
    (call :greet/hello name)))`,
		"slow": `(service slow
  (on (spin) "Never ends."
    (loop [] (recur)))
  (on (fast) "Ends."
    (ok :fast)))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// kind is the :kind of an err, or the value of an ok.
	kind := func(call string) string {
		return `(match ` + call + ` [(err {:kind k}) k] [(ok v) v])`
	}
	tests := []struct {
		src, want string
	}{
		{`(call :greet/hello "ann")`, `(ok "hello, ann")`},
		{`(call :greet/both "ann" "bob")`, `(ok ["hello, ann" "hello, bob"])`},
		{`(call :relay/hello "ann")`, `(ok "hello, ann")`},
		{`(call :greet/refuse)`, `(err {:kind :nope})`},
		{`(call :greet/cwd)`, `(ok "/tmp/w")`},
		{kind(`(call :nobody/hello)`), `:no-address`},
		{kind(`(call :greet/nope)`), `:no-function`},
		{kind(`(call :greet/hello)`), `:bad-call`},
		{kind(`(call :greet/boom)`), `:fault`},
		{kind(`(call :greet/plain)`), `:fault`},
		{kind(`(call :greet/leak)`), `:fault`},
		// A timeout cancels the handler, so the service is free again.
		{kind(`(call :slow/spin)`), `:timeout`},
		{`(call :slow/fast)`, `(ok :fast)`},
		// Helpers aren't functions of the service.
		{kind(`(call :greet/greeting "ann")`), `:no-function`},
	}
	for _, tt := range tests {
		got, err := c.Eval(tt.src, "/tmp/w")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if got.Value != tt.want || got.Error != "" {
			t.Errorf("%s = %s %s, want %s", tt.src, got.Value, got.Error, tt.want)
		}
	}

	faults := []struct{ src, want string }{
		{`(call :greet/hello (fn (x) x))`, "eval:1:1: call: can't send [#<fn>]: only data travels"},
		{`(call :greet "ann")`, "eval:1:1: call: :greet has no address: only a service can leave it out, want :address/fn"},
		{`(call :/x)`, "eval:1:1: call: want a target :address/fn, got :/x"},
		{`(call "greet/hello")`, `eval:1:1: call: want a target :address/fn, got "greet/hello"`},
	}
	for _, tt := range faults {
		got, err := c.Eval(tt.src, "")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if !got.Fault || got.Error != tt.want {
			t.Errorf("%s = %+v, want fault %q", tt.src, got, tt.want)
		}
	}
}

func TestSend(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	out := filepath.Join(dir, "out")
	writeServices(t, dir, map[string]string{
		"notes": `(service notes
  (on (write path text) "Writes text to path."
    (file/write path text))
  (on (now path) "Writes to path through itself."
    (send :write path "now"))
  (on (later path) "Writes to path through itself, in a moment."
    (send-after 20ms :write path "later")))`,
	})
	start(t, dir)
	c := dial(t, dir)

	tests := []struct {
		src, want, file string
	}{
		{`(send :notes/write "` + out + `" "sent")`, "(ok :done)", "sent"},
		{`(send :notes/now "` + out + `")`, "(ok :done)", "now"},
		{`(send :notes/later "` + out + `")`, "(ok :done)", "later"},
		{`(send-after 20ms :notes/write "` + out + `" "after")`, "(ok :done)", "after"},
		{`(match (send :nobody/write) [(err {:kind k}) k])`, ":no-address", ""},
	}
	for _, tt := range tests {
		got, err := c.Eval(tt.src, "")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if got.Value != tt.want || got.Error != "" {
			t.Errorf("%s = %s %s, want %s", tt.src, got.Value, got.Error, tt.want)
		}
		if tt.file == "" {
			continue
		}
		// The send is done when the file holds what it wrote.
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			b, _ := os.ReadFile(out)
			if string(b) == tt.file {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: file holds %q, want %q", tt.src, b, tt.file)
			}
		}
	}

	got, err := c.Eval(`(send-after 1 :notes/write)`, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "eval:1:1: send-after: want a duration of 0s or more, got 1"; !got.Fault || got.Error != want {
		t.Errorf("send-after 1 = %+v, want fault %q", got, want)
	}
}

func TestCycle(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"a": `(service a
  (on (ping) "Calls b, which calls back." (call :b/pong))
  (on (itself) "Calls itself." (call :itself))
  (on (fine) "Answers." (ok :fine)))`,
		"b": `(service b
  (on (pong) "Calls a." (call :a/ping))
  (on (fine) "Calls a, which isn't waiting." (call :a/fine)))`,
	})
	start(t, dir)
	c := dial(t, dir)

	// The default 5s timeout stays: a cycle must fail at once, not time out.
	tests := []struct{ src, want string }{
		{`(call :a/ping)`, `(err {:kind :cycle :message ":a/ping would wait for itself: :a -> :b -> :a"})`},
		{`(call :a/itself)`, `(err {:kind :cycle :message ":a/itself would wait for itself: :a -> :a"})`},
		{`(call :b/fine)`, `(ok :fine)`},
	}
	for _, tt := range tests {
		begin := time.Now()
		got, err := c.Eval(tt.src, "")
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if got.Value != tt.want || got.Error != "" {
			t.Errorf("%s = %s %s, want %s", tt.src, got.Value, got.Error, tt.want)
		}
		if d := time.Since(begin); d > time.Second {
			t.Errorf("%s took %v", tt.src, d)
		}
	}
}

func TestLoadService(t *testing.T) {
	dir := tempDir(t)
	write := func(name, src string) string {
		path := filepath.Join(dir, name+".puff")
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name, src, want string
	}{
		{"ok", `(service ok (on (f) "Doc." (ok 1)))`, ""},
		{"read", `(service read`, ":1:1: no closing ')'"},
		{"two", "(service two)\n(def x 1)", ":1:1: a service file holds one (service name ...)"},
		{"bare", `(def x 1)`, ":1:1: a service file holds one (service name ...)"},
		{"named", `(service other)`, ":1:1: service wants its name, named, as in its file name"},
		{"option", `(service option :begin 1)`, ":1:17: unknown service option :begin"},
		{"start", `(service start :start (fn () (ok 1)) (on (f) "Doc." (ok 1)))`, ""},
		{"startnofn", `(service startnofn :start)`, ":1:20: :start wants a function"},
		{"starttwice", `(service starttwice :start (fn () (ok 1)) :start (fn () (ok 2)))`, ":1:43: service takes :start once"},
		{"startnotfn", `(service startnotfn :start (def x 1))`, ":1:28: :start wants a function"},
		{"starterr", `(service starterr :start (fn () (err {:kind :nope})))`, ":1:26: :start returned (err {:kind :nope}), want (ok v)"},
		{"startvalue", `(service startvalue :start (fn () 5))`, ":1:28: :start returned 5, want (ok v)"},
		{"nodoc", `(service nodoc (on (f) (ok 1)))`, ":1:16: on wants (fn params...), a doc string and a body"},
		{"marked", `(service marked (on (f) :write "Doc." (ok 1)))`, ""},
		{"badmark", `(service badmark (on (f) :rw "Doc." (ok 1)))`, ":1:18: on takes :read or :write, and :update, after (fn params...), got :rw"},
		{"markstwice", `(service markstwice (on (f) :read :write "Doc." (ok 1)))`, ":1:21: on takes :read or :write, and :update, after (fn params...), got :write"},
		{"updatestateless", `(service updatestateless (on (f) :update "Doc." (ok 1)))`, ":1:26: :update needs a service with :state"},
		{"subupdate", `(service subupdate (on :t (v) :update (ok v)))`, ":1:20: :update needs a service with :state"},
		{"askupdate", `(service askupdate (ask (f) :update "Doc."))`, ":1:20: an ask has no state to :update"},
		{"statenone", `(service statenone :state)`, ":1:20: :state wants its first state"},
		{"statetwice", `(service statetwice :state 0 :state 1)`, ":1:30: service takes :state once"},
		{"statenoparam", `(service statenoparam :state 0 (on (f) "Doc." (ok 1)))`, ":1:32: on in a service with :state wants (fn state params...)"},
		{"statesub", `(service statesub :state 0 (on :t (v) (ok v)))`, ":1:28: on :topic in a service with :state wants (state v) and a body"},
		{"marknodoc", `(service marknodoc (on (f) :read (ok 1)))`, ":1:20: on wants (fn params...), a doc string and a body"},
		{"sub", `(service sub (on :t (v) (ok v)))`, ""},
		{"subnoparam", `(service subnoparam (on :t (ok 1)))`, ":1:21: on :topic wants (v), one parameter for the value, and a body"},
		{"subtwo", `(service subtwo (on :t (a b) (ok 1)))`, ":1:17: on :topic wants (v), one parameter for the value, and a body"},
		{"subtwice", `(service subtwice (on :t (v) (ok 1)) (on :t (v) (ok 2)))`, ":1:38: already subscribed to :t at"},
		{"subname", `(service subname (on "t" (v) (ok 1)))`, `:1:18: on wants :topic or (fn params...), got "t"`},
		{"subfault", `(service subfault (on :t (v) (nope v)))`, ":1:31: unknown name nope"},
		{"nodoc2", `(service nodoc2 (on (f) (ok 1) (ok 2)))`, ":1:17: on wants a doc string after (fn params...)"},
		{"unknown", `(service unknown (on (f) "Doc." (nope)))`, ":1:34: unknown name nope"},
		{"twice", `(service twice (on (f) "Doc." (ok 1)) (on (f) "Doc." (ok 2)))`, ":1:44: f is already defined at"},
		{"fault", `(service fault (def x (+ 1 1.0)))`, ":1:23: +: can't add int and float"},
		{"forever", `(service forever (def x (loop [] (recur))))`, "cancelled"},
		{"agent", `(agent agent :prompt "Watch things.")`, ""},
		{"agentall", `(agent agentall :prompt "p" :model "opus")`, ""},
		{"agentnamed", `(agent other :prompt "p")`, ":1:1: service wants its name, agentnamed, as in its file name"},
		{"agentnoprompt", `(agent agentnoprompt :model "opus")`, ":1:1: agent wants :prompt, a string saying what it is for"},
		{"agentopt", `(agent agentopt :prompt "p" :tools [:gh/prs])`, ":1:29: unknown agent option :tools; it takes :prompt, :model, :can, :dir, :limits and :every"},
		{"agentlimits", `(agent agentlimits :prompt "p" :limits {:day 2 :time 5m})`, ""},
		{"agentlimitsbad", `(agent agentlimitsbad :prompt "p" :limits {:day "lots"})`, ":1:35: :limits wants {:turn usd :day usd :time d :tasks n}, any of them: dollars a turn and a day, how long a turn may run, and how many tasks it may have open"},
		{"agentlimitskey", `(agent agentlimitskey :prompt "p" :limits {:turns 3})`, ":1:35: :limits wants {:turn usd :day usd :time d :tasks n}, any of them: dollars a turn and a day, how long a turn may run, and how many tasks it may have open"},
		{"agentlimitszero", `(agent agentlimitszero :prompt "p" :limits {:turn 0})`, ":1:36: :limits wants {:turn usd :day usd :time d :tasks n}, any of them: dollars a turn and a day, how long a turn may run, and how many tasks it may have open"},
		{"agentdir", `(agent agentdir :prompt "p" :dir ".")`, ""},
		{"agentnodir", `(agent agentnodir :prompt "p" :dir "nope")`, ":1:36: :dir: no directory "},
		{"agentcan", `(agent agentcan :prompt "p" :can [:gh/prs :todo/add])`, ""},
		{"agentcanbare", `(agent agentcanbare :prompt "p" :can [:gh])`, ":1:33: :can wants the functions the agent may use, as [:gh/prs :todo/add]"},
		{"agentcanstr", `(agent agentcanstr :prompt "p" :can "gh/prs")`, ":1:32: :can wants the functions the agent may use, as [:gh/prs :todo/add]"},
		{"agenttwice", `(agent agenttwice :prompt "p" :prompt "q")`, ":1:31: agent takes :prompt once"},
		{"agentevery", `(agent agentevery :prompt "p" :every "1d")`, ":1:31: :every wants a duration, as 1d"},
		{"agentnone", `(agent agentnone :prompt)`, ":1:18: :prompt wants a string, what the agent is for"},
		{"agentbody", `(agent agentbody :prompt "p" (def x 1))`, `:1:30: agent takes options, as :prompt "...", not (def x 1)`},
	}
	set(t, &loadTimeout, 100*time.Millisecond)
	d := &daemon{ctx: context.Background()}
	for _, tt := range tests {
		path := write(tt.name, tt.src)
		_, err := d.loadService(path)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// A panic in a handler faults its message rather than ending the daemon,
// and arguments that aren't a vector are a bad call, not a panic.
func TestHandlePanic(t *testing.T) {
	t.Parallel()
	s := &service{name: "boom", d: &daemon{callTimeout: time.Second}, fns: map[string]handler{
		"go": {native: func(message, []puff.Value) (puff.Value, error) { panic("oops") }},
	}}
	r, faulted, stopped := s.handle(context.Background(), message{fn: "go", args: "[]"})
	if want := `(err {:kind :fault :message ":boom/go panicked: oops"})`; r != want || !faulted || stopped {
		t.Errorf("a panicking handler = %s %v %v, want %s, faulted", r, faulted, stopped, want)
	}
	r, faulted, _ = s.handle(context.Background(), message{fn: "go", args: "5"})
	if !strings.Contains(r, ":kind :bad-call") || faulted {
		t.Errorf("arguments 5 = %s %v, want a bad call", r, faulted)
	}
}
