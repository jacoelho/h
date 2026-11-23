package mw_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jacoelho/h/mw"
)

func TestRequestID(t *testing.T) {
	var seen string
	handler := mw.RequestID("X-Request-Id")(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		seen, _ = mw.RequestIDFrom(req.Context())
	}))
	cases := []struct {
		name, in string
		kept     bool
	}{
		{"kept from the request", "abc-123", true},
		{"made when absent", "", false},
		{"made when too long", strings.Repeat("x", 129), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if c.in != "" {
				req.Header.Set("X-Request-Id", c.in)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			echoed := w.Header().Get("X-Request-Id")
			if echoed == "" || echoed != seen {
				t.Fatalf("echoed %q, context %q", echoed, seen)
			}
			if (seen == c.in) != c.kept {
				t.Fatalf("id %q from input %q", seen, c.in)
			}
		})
	}
}

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	})
	handler := mw.RequestID("X-Request-Id")(mw.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, req.Pattern = mux.Handler(req)
		mux.ServeHTTP(w, req)
	})))
	req := httptest.NewRequest("GET", "/items/7", nil)
	req.Header.Set("X-Request-Id", "r1")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	line := buf.String()
	for _, want := range []string{"method=GET", `pattern="GET /items/{id}"`, "path=/items/7", "status=418", "bytes=5", "request_id=r1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %s", line, want)
		}
	}
}

func TestAccessLogWriterSupportsResponseController(t *testing.T) {
	var flushErr error
	handler := mw.AccessLog(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if flushErr != nil {
		t.Fatalf("Flush through the access log writer: %v", flushErr)
	}
}

func TestTimeoutSetsDeadline(t *testing.T) {
	var left time.Duration
	handler := mw.Timeout(time.Minute)(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		d, ok := req.Context().Deadline()
		if ok {
			left = time.Until(d)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if left <= 50*time.Second || left > time.Minute {
		t.Fatalf("deadline %v away, want about a minute", left)
	}
}
