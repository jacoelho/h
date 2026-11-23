package h_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jacoelho/h"
)

type payload struct {
	ID    string   `json:"id"`
	Notes string   `json:"notes"`
	Tags  []string `json:"tags"`
}

func kibPayload() []byte {
	b, _ := json.Marshal(payload{ID: "p-1", Notes: strings.Repeat("n", 960), Tags: []string{"a", "b", "c"}})
	return b
}

func passThrough(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { next.ServeHTTP(w, req) })
}

func benchServe(b *testing.B, handler http.Handler, body []byte, accept string) {
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest("POST", "/p", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// BenchmarkB1Adapter compares the router with a hand-written handler doing
// the same work: cross-origin check, bounded decode, call, and a buffered
// encode sent with Content-Length. The difference is the adapter's overhead.
func BenchmarkB1Adapter(b *testing.B) {
	body := kibPayload()
	r, _ := h.New(h.Config{})
	r.Use(passThrough)
	r.Post("/p", h.Func(func(_ context.Context, p payload) (payload, error) { return p, nil }))
	router := handlerOf(b, r)
	b.Run("router", func(b *testing.B) { benchServe(b, router, body, "") })
	b.Run("router-accept-any", func(b *testing.B) { benchServe(b, router, body, "*/*") })
	b.Run("baseline", func(b *testing.B) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /p", func(w http.ResponseWriter, req *http.Request) {
			var p payload
			if err := json.UnmarshalRead(http.MaxBytesReader(w, req.Body, 1<<20), &p); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var buf bytes.Buffer
			if err := json.MarshalWrite(&buf, p); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			_, _ = w.Write(buf.Bytes())
		})
		benchServe(b, http.NewCrossOriginProtection().Handler(passThrough(mux)), body, "")
	})
}

// BenchmarkB2LargeResponse reports allocation for a 4 MiB JSON response,
// which streams past SpillAt rather than buffering whole.
func BenchmarkB2LargeResponse(b *testing.B) {
	big := make([]payload, 4096)
	for i := range big {
		big[i] = payload{ID: fmt.Sprint(i), Notes: strings.Repeat("n", 1000)}
	}
	r, _ := h.New(h.Config{})
	r.Get("/big", func(context.Context, h.Request[h.Empty]) (h.Response[[]payload], error) { return h.OK(big), nil })
	handler := handlerOf(b, r)
	b.ReportAllocs()
	for b.Loop() {
		w := discardWriter{header: http.Header{}}
		handler.ServeHTTP(&w, httptest.NewRequest("GET", "/big", nil))
		if w.status != 200 {
			b.Fatalf("status %d", w.status)
		}
	}
}

type discardWriter struct {
	header http.Header
	status int
}

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) WriteHeader(code int)        { d.status = code }
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// BenchmarkB3PatternLookup measures the extra ServeMux match the router does
// so root middleware sees req.Pattern, on a 50-route mux.
func BenchmarkB3PatternLookup(b *testing.B) {
	mux := http.NewServeMux()
	for i := range 50 {
		mux.HandleFunc(fmt.Sprintf("GET /r%d/{id}", i), func(http.ResponseWriter, *http.Request) {})
	}
	req := httptest.NewRequest("GET", "/r42/7", nil)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = mux.Handler(req)
	}
}
