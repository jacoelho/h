package mw_test

import (
	"bytes"
	"fmt"
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
		{"kept at 128 bytes", strings.Repeat("x", 128), true},
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
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	})
	handler := mw.RequestID("X-Request-Id")(mw.AccessLog(log, slog.LevelDebug)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, req.Pattern = mux.Handler(req)
		mux.ServeHTTP(w, req)
	})))
	req := httptest.NewRequest("GET", "/items/7", nil)
	req.Header.Set("X-Request-Id", "r1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Fatalf("client got status %d through AccessLog, want 418", w.Code)
	}
	line := buf.String()
	for _, want := range []string{"level=DEBUG", "method=GET", `pattern="GET /items/{id}"`, "path=/items/7", "status=418", "bytes=5", "request_id=r1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %s", line, want)
		}
	}
}

func TestAccessLogWriterSupportsResponseController(t *testing.T) {
	var flushErr error
	handler := mw.AccessLog(slog.New(slog.DiscardHandler), slog.LevelInfo)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestAccessLogWithoutALevelLogsAtInfo(t *testing.T) {
	var buf bytes.Buffer
	handler := mw.AccessLog(slog.New(slog.NewTextHandler(&buf, nil)), nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "status=200") {
		t.Fatalf("log %q, want an info line with status 200", buf.String())
	}
}

func TestAccessLogRecordsTheFirstFinalStatus(t *testing.T) {
	for name, c := range map[string]struct {
		handler http.HandlerFunc
		want    int
	}{
		"informational first": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK},
		"body first": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := mw.AccessLog(slog.New(slog.NewTextHandler(&buf, nil)), nil)(c.handler)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
			if want := fmt.Sprintf("status=%d", c.want); !strings.Contains(buf.String(), want) {
				t.Fatalf("log %q, want %s, the final status net/http sends", buf.String(), want)
			}
		})
	}
}
