package h_test

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
)

func newRouter(t *testing.T, cfg h.Config) *h.Router {
	t.Helper()
	r, err := h.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type call struct {
	method string
	target string
	body   string
	header http.Header
}

func handlerOf(tb testing.TB, target any) http.Handler {
	tb.Helper()
	switch x := target.(type) {
	case *h.Router:
		hd, err := x.Handler()
		if err != nil {
			tb.Fatalf("Handler: %v", err)
		}
		return hd
	case http.Handler:
		return x
	}
	tb.Fatalf("cannot serve %T", target)
	return nil
}

func send(tb testing.TB, target any, c call) *htest.Response {
	tb.Helper()
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req := httptest.NewRequestWithContext(tb.Context(), c.method, c.target, body)
	maps.Copy(req.Header, c.header)
	return htest.ForHandler(tb, handlerOf(tb, target)).Send(req)
}

func jsonHeader() http.Header { return http.Header{"Content-Type": {"application/json"}} }

func wantJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	g, w := jsontext.Value(got), jsontext.Value(want)
	if err := g.Canonicalize(); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := w.Canonicalize(); err != nil {
		t.Fatalf("want invalid JSON %q: %v", want, err)
	}
	if string(g) != string(w) {
		t.Fatalf("JSON\n got %s\nwant %s", g, w)
	}
}

type markerKey struct{}

func addHeader(key, value string) h.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Add(key, value)
			next.ServeHTTP(w, req)
		})
	}
}

func wantSetupError(t *testing.T, r *h.Router, contains string) {
	t.Helper()
	_, err := r.Handler()
	if !errors.Is(err, h.ErrSetup) {
		t.Fatalf("Handler error %v, want ErrSetup", err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("Handler error %q does not mention %q", err, contains)
	}
}
