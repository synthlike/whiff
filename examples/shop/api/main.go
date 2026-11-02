// Command api is the shop's API, a demo for docs/procs.md: orders, kept in
// redis, on :8080. /health answers 200 once postgres and redis take
// connections. SIGTERM shuts it down cleanly.
//
//	POST /orders {"item": "tea", "qty": 2}  201 {"id": 1, "item": "tea", "qty": 2}
//	GET  /orders/1                          200 the order, or 404
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	postgres = "localhost:5432"
	redis    = "localhost:6379"
)

type order struct {
	ID   int64  `json:"id"`
	Item string `json:"item"`
	Qty  int    `json:"qty"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("POST /orders", create)
	mux.HandleFunc("GET /orders/{id}", get)
	srv := &http.Server{Addr: "localhost:8080", Handler: logged(mux)}
	go func() {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		<-ctx.Done()
		log.Print("SIGTERM: shutting down")
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("api listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("api stopped")
}

func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func health(w http.ResponseWriter, r *http.Request) {
	for _, addr := range []string{postgres, redis} {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		c.Close()
	}
	fmt.Fprintln(w, "ok")
}

func create(w http.ResponseWriter, r *http.Request) {
	var o order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil || o.Item == "" || o.Qty < 1 {
		http.Error(w, `want {"item": "...", "qty": n}, n from 1`, http.StatusBadRequest)
		return
	}
	id, err := cmd("INCR", "orders")
	if err == nil {
		o.ID, _ = strconv.ParseInt(id, 10, 64)
		b, _ := json.Marshal(o)
		_, err = cmd("SET", "order:"+id, string(b))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(o)
}

func get(w http.ResponseWriter, r *http.Request) {
	b, err := cmd("GET", "order:"+r.PathValue("id"))
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case b == "":
		http.NotFound(w, r)
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, b)
	}
}

// cmd sends redis one command, in RESP, and returns its answer: an
// integer's digits, a string, or "" for none.
func cmd(args ...string) (string, error) {
	c, err := net.DialTimeout("tcp", redis, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	fmt.Fprintf(c, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(c, "$%d\r\n%s\r\n", len(a), a)
	}
	rd := bufio.NewReader(c)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case line == "":
		return "", fmt.Errorf("redis: empty answer")
	case line[0] == '-':
		return "", fmt.Errorf("redis: %s", line[1:])
	case line[0] == '+', line[0] == ':':
		return line[1:], nil
	case line == "$-1":
		return "", nil
	case line[0] == '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", err
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(rd, b); err != nil {
			return "", err
		}
		return string(b[:n]), nil
	}
	return "", fmt.Errorf("redis: %q", line)
}
