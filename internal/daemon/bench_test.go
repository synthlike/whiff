package daemon

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/synthlike/whiff/internal/rpc"
)

// BenchmarkMessages measures a message's way through whiff: put on the
// route goroutine, the service's mailbox, service.run and history, to a
// service that answers at once. One daemon serves every case, so sniff
// can follow the whole run:
//
//	go test -c -o /tmp/sniff.test ./cmd/sniff
//	SNIFF_TEST_DIR=/tmp/whiff-bench /tmp/sniff.test -open   # in one shell
//	WHIFF_BENCH_DIR=/tmp/whiff-bench go test ./internal/daemon -run '^$' -bench Messages -benchtime 5s
//
// With WHIFF_BENCH_DIR the daemon runs there, and waits a moment first
// for sniff, which looks for it every second, to connect. A page that
// can't keep up drops entries and reopens, and every client following
// history costs the route goroutine a little, so the numbers with sniff
// open are a bit lower.
func BenchmarkMessages(b *testing.B) {
	dir := os.Getenv("WHIFF_BENCH_DIR")
	if dir == "" {
		dir = tempDir(b)
	} else if err := rpc.PrepareDir(dir); err != nil {
		b.Fatal(err)
	}
	writeServices(b, dir, map[string]string{
		"bench": `(service bench (on (echo x) "Answers x." (ok x)))`,
	})
	start(b, dir)
	c := dial(b, dir)
	if os.Getenv("WHIFF_BENCH_DIR") != "" {
		time.Sleep(2 * time.Second)
	}
	eval := func(b *testing.B, src string) string {
		b.Helper()
		resp, err := c.Eval(src, "")
		if err != nil || resp.Error != "" {
			b.Fatalf("%s: %+v %v", src, resp, err)
		}
		return resp.Value
	}

	// A client's call: the socket and JSON both ways, on top of the rest.
	b.Run("call/client", func(b *testing.B) {
		for i := range b.N {
			if _, err := c.Message("call", "bench/echo", fmt.Sprintf("[%d]", i), ""); err != nil {
				b.Fatal(err)
			}
		}
		perSecond(b)
	})

	// Calls from puff, in one eval: put, service.run and history alone.
	b.Run("call/puff", func(b *testing.B) {
		eval(b, fmt.Sprintf(`(def (run n)
  (loop [i 0]
    (if (= i n)
      (ok i)
      (do (try (call :bench/echo i)) (recur (+ i 1))))))
(run %d)`, b.N))
		perSecond(b)
	})

	// Clients calling at once: the one service, and the route goroutine,
	// take them one at a time.
	b.Run("call/parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			pc, err := rpc.Dial(dir, nil)
			if err != nil {
				b.Error(err)
				return
			}
			defer pc.Close()
			for pb.Next() {
				if _, err := pc.Message("call", "bench/echo", "[0]", ""); err != nil {
					b.Error(err)
					return
				}
			}
		})
		perSecond(b)
	})

	// Sends from puff, without waiting, then a call that waits for them
	// all, once there is room for it: the mailbox takes 100 at most, and a
	// send to a full one fails at once, counted as full/op.
	b.Run("send/puff", func(b *testing.B) {
		v := eval(b, fmt.Sprintf(`(def (drain)
  (loop []
    (match (call :bench/echo 0)
      [(err {:kind :full}) (recur)]
      [r r])))
(def (run n)
  (loop [i 0 full 0]
    (if (= i n)
      (do (try (drain)) (ok full))
      (match (send :bench/echo i)
        [(err {:kind :full}) (recur (+ i 1) (+ full 1))]
        [_ (recur (+ i 1) full)]))))
(run %d)`, b.N))
		var full int
		if _, err := fmt.Sscanf(strings.TrimPrefix(v, "(ok "), "%d", &full); err != nil {
			b.Fatalf("run = %s", v)
		}
		b.ReportMetric(float64(full)/float64(b.N), "full/op")
		perSecond(b)
	})
}

// perSecond reports b's messages per second.
func perSecond(b *testing.B) {
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "msgs/s")
}
