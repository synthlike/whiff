package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/synthlike/whiff/internal/puff"
	"github.com/synthlike/whiff/internal/sandbox"
)

// TestSandboxJob runs :sandbox jobs: bad options are refused anywhere;
// where there's a sandbox, the job runs in Linux, writes only its dir, and
// a cancel ends its container; where there isn't, it fails to start.
func TestSandboxJob(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{})
	start(t, dir)
	c := dial(t, dir)
	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, dir)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return resp.Value + resp.Error
	}
	for src, want := range map[string]string{
		`(call :jobs/run ["true"] {:sandbox {:net 1}})`:                 "want :sandbox #t, or {:net #t}",
		`(call :jobs/run ["true"] {:sandbox #t :session [:todo/open]})`: ":session and :sandbox don't go together",
	} {
		if got := eval(src); !strings.Contains(got, want) {
			t.Errorf("%s = %s, want it to say %q", src, got, want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := sandbox.New(ctx, false); err != nil {
		got := eval(`(call :jobs/run ["true"] {:sandbox #t})`)
		if !strings.Contains(got, ":kind :start") {
			t.Errorf("without a sandbox, a :sandbox job = %s; want it not to start", got)
		}
		t.Skipf("the rest needs a sandbox: %v", err)
	}
	wait := func(id string) string {
		t.Helper()
		resp, err := c.Message("wait", "", "["+id+"]", "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Value + resp.Error
	}
	pending := func(src string) string {
		t.Helper()
		return eval(`(match ` + src + ` [(err {:kind :pending :id id}) id])`)
	}
	id := pending(`(call :jobs/run ["sh" "-c" "echo boxed > f; uname -s"] {:sandbox #t :as :text})`)
	if got := wait(id); got != `(ok "Linux\n")` {
		t.Errorf("a sandboxed job = %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "f")); err != nil || string(b) != "boxed\n" {
		t.Errorf("its dir has %q %v", b, err)
	}

	id = pending(`(call :jobs/run ["sleep" "300"] {:sandbox #t})`)
	list := eval(`(match (call :jobs/list) [(ok [{:container n}]) n])`)
	box := strings.Trim(list, `"`)
	if !strings.HasPrefix(box, "whiff-job-") {
		t.Fatalf(":jobs/list's container = %s", list)
	}
	host, err := sandbox.Host()
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "its container", func() string { return strconv.FormatBool(host.Gone(ctx, box)) }, "false")
	eval(`(call :jobs/cancel ` + id + `)`)
	if got := wait(id); !strings.Contains(got, ":kind :cancelled") {
		t.Errorf("a cancelled sandboxed job = %s", got)
	}
	if !host.Gone(ctx, box) {
		host.End(ctx, box)
		t.Errorf("container %s outlived its cancelled job", box)
	}
}

// TestSandboxStray: a sandboxed job's container that still runs after
// whiff died is ended at start, as its name, unlike a pid, can't be
// another's; the job is then lost as one that runs no more.
func TestSandboxStray(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := sandbox.New(ctx, false)
	if err != nil {
		t.Skip(err)
	}
	dir := tempDir(t)
	got := filepath.Join(dir, "got")
	writeServices(t, dir, map[string]string{
		"waiter": `(service waiter
  (on (whiff/done id r) "Notes the answer." (file/append "` + got + `" (string/concat (int/to-string id) " " (value/to-string r) "\n"))))`,
	})
	done := start(t, dir)
	cl := dial(t, dir)
	stopDaemon(t, cl, done)

	// What whiff dying leaves: the job stored as running, its client and
	// container still there.
	box := sandbox.Name("job")
	client := c.Command(ctx, box, dir, []string{"sleep", "300"})
	client.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { client.Wait(); close(reaped) }()
	t.Cleanup(func() { c.End(context.Background(), box); <-reaped })
	eventually(t, "the container", func() string { return strconv.FormatBool(c.Gone(ctx, box)) }, "false")
	j := &job{ticket: ticket{id: 900, from: "waiter", user: true}, cmd: puff.NewVector(puff.String("sleep")),
		pid: client.Process.Pid, box: box, opts: jobOpts{sandbox: true}}
	j.attempts = []attempt{{started: time.Now(), pid: j.pid, box: box}}
	withDB(t, dir, func(db *bbolt.DB) {
		err := db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(tasksBucket).Put(idKey(j.id), []byte(puff.Print(j.stored())))
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	start(t, dir)
	cl = dial(t, dir)
	eventually(t, "the lost job's answer", func() string { b, _ := os.ReadFile(got); return string(b) },
		`900 (err {:kind :interrupted :message "job 900 was running when whiff died: how far it got is unknown" :outcome :unknown})`+"\n")
	if !c.Gone(ctx, box) {
		t.Errorf("container %s still runs", box)
	}
	historyHas(t, cl, "job 900's container "+box+" still ran after whiff died, so whiff ended it")
}
