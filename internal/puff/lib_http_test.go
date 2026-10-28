package puff

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPLib(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Method", r.Method)
			fmt.Fprintf(w, "%s %s", r.Header.Get("Authorization"), b)
		case "/slow":
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	u := srv.URL
	testLib(t, []struct{ src, want string }{
		{fmt.Sprintf(`(http/get %q)`, u+"/echo"), `(ok " ")`},
		{fmt.Sprintf(`(http/get %q {:headers {"Authorization" "token x"}})`, u+"/echo"), `(ok "token x ")`},
		{fmt.Sprintf(`(http/get %q)`, u+"/missing"), fmt.Sprintf(`(err {:body "nope\n" :kind :status :message "GET %s/missing: 404 Not Found" :status 404})`, u)},
		{fmt.Sprintf(`(match (http/request %q {:method :post :body "hi" :headers {:authorization "a"}}) [(ok {:status s :body b :headers {"x-method" m}}) [s b m]])`, u+"/echo"), `[200 "a hi" "POST"]`},
		{fmt.Sprintf(`(match (http/request %q) [(ok {:status s}) s])`, u+"/missing"), `404`},

		{`(http/get "ftp://example.com")`, `1:1: http/get: want an http or https url, got "ftp://example.com"`},
		{`(http/get "/echo")`, `1:1: http/get: want an http or https url, got "/echo"`},
		{fmt.Sprintf(`(http/get %q {:body "x"})`, u), "1:1: http/get: unknown option :body"},
		{fmt.Sprintf(`(http/request %q {:method "post"})`, u), `1:1: http/request: want a :method keyword, like :post, got "post"`},
		{fmt.Sprintf(`(http/request %q {:headers {"a" 1}})`, u), "1:1: http/request: want header a's value as a string, got 1"},
	})

	got := load(NewEnv(), fmt.Sprintf(`(http/get %q {:timeout 100ms})`, u+"/slow"))
	if want := fmt.Sprintf(`(err {:kind :timeout :message "GET %s/slow took longer than 100ms"})`, u); got != want {
		t.Errorf("timeout: %s, want %s", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, want := loadCtx(ctx, NewEnv(), fmt.Sprintf(`(http/get %q)`, u+"/slow")), "1:1: http/get: cancelled"; got != want {
		t.Errorf("cancelled: %s, want %s", got, want)
	}

	srv.Close()
	if got := load(NewEnv(), fmt.Sprintf(`(http/get %q)`, u)); !strings.HasPrefix(got, "(err {:kind :http") {
		t.Errorf("nobody listening: %s", got)
	}
}
