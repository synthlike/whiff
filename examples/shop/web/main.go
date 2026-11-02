// Command web is the shop's web UI, a demo for docs/procs.md: a page on
// :3000 that shows an order from the API, which it asks on :8080. SIGTERM
// shuts it down cleanly.
package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("order")
		if id == "" {
			id = "1"
		}
		resp, err := http.Get("http://localhost:8080/orders/" + id)
		var body string
		if err != nil {
			body = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = resp.Status + ": " + string(b)
		}
		fmt.Fprintf(w, "<!doctype html><title>shop</title><h1>shop</h1><p>Order %s: <code>%s</code></p>",
			html.EscapeString(id), html.EscapeString(body))
	})
	srv := &http.Server{Addr: "localhost:3000", Handler: mux}
	go func() {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		<-ctx.Done()
		log.Print("SIGTERM: shutting down")
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("web listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("web stopped")
}
