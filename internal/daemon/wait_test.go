package daemon

import (
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

func TestWait(t *testing.T) {
	dir := tempDir(t)
	writeServices(t, dir, map[string]string{
		"todo": `(service todo
  (ask (suggest text) "Suggests a todo.")
  (on (answer id v) "Answers id." (reply id (ok v))))`,
	})
	start(t, dir)
	c := dial(t, dir)

	eval := func(src string) string {
		t.Helper()
		resp, err := c.Eval(src, dir)
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", src, err, resp.Error)
		}
		return resp.Value
	}
	// waiting waits for id on a connection of its own.
	waiting := func(id string) <-chan rpc.Response {
		got := make(chan rpc.Response, 1)
		w := dial(t, dir)
		go func() {
			resp, err := w.Message("wait", "", "["+id+"]", "")
			if err != nil {
				resp.Error = err.Error()
			}
			got <- resp
		}()
		return got
	}
	answer := func(got <-chan rpc.Response) string {
		t.Helper()
		select {
		case resp := <-got:
			return resp.Value + resp.Error
		case <-time.After(2 * time.Second):
			t.Fatal("wait didn't return")
			return ""
		}
	}

	id := eval(`(match (call :todo/suggest "docs") [(err {:kind :pending :id id}) id])`)
	// Two waits on the same id, before it is answered, both get it.
	first, second := waiting(id), waiting(id)
	time.Sleep(50 * time.Millisecond)
	eval(`(call :todo/answer ` + id + ` "yes")`)
	for _, got := range []<-chan rpc.Response{first, second} {
		if r := answer(got); r != `(ok "yes")` {
			t.Errorf("wait %s = %s, want (ok \"yes\")", id, r)
		}
	}
	// A wait after the answer gets it at once.
	if r := answer(waiting(id)); r != `(ok "yes")` {
		t.Errorf("wait %s after the answer = %s", id, r)
	}

	job := eval(`(match (call :jobs/run ["echo" "hi"]) [(err {:kind :pending :id id}) id])`)
	if r := answer(waiting(job)); r != `(ok {:exit 0 :stderr "" :stdout "hi\n"})` {
		t.Errorf("wait for job %s = %s", job, r)
	}

	if r := answer(waiting("99")); r != `(err {:kind :no-id :message "no task 99 is pending, or ended in the last 30 days"})` {
		t.Errorf("wait 99 = %s", r)
	}
	if r := answer(waiting(`"x"`)); r != `want [id], got ["x"]` {
		t.Errorf(`wait "x" = %s`, r)
	}
}
