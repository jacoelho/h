package h_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/mw"
)

func TestStalledBodyTimesOut(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(mw.Timeout(100 * time.Millisecond))
	r.Post("/items", echo(nil))
	s := serve(t, r)

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /items HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"name\":")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	status, err := readStatusLine(conn)
	if err != nil {
		t.Fatalf("no response within 3s: %v", err)
	}
	if status != "HTTP/1.1 408 Request Timeout" {
		t.Fatalf("status line %q", status)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("408 took %v", elapsed)
	}
}

func readStatusLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return b.String(), err
		}
		if buf[0] == '\n' {
			return strings.TrimSuffix(b.String(), "\r"), nil
		}
		b.WriteByte(buf[0])
	}
}

// deadlineOnly reports a deadline without ever expiring itself, so the test
// observes only cancellations caused by the connection.
type deadlineOnly struct {
	context.Context
	at time.Time
}

func withReportedDeadline(parent context.Context, at time.Time) context.Context {
	return deadlineOnly{parent, at}
}

func (d deadlineOnly) Deadline() (time.Time, bool) { return d.at, true }

func TestFullyReadBodyKeepsContextPastDeadline(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := withReportedDeadline(req.Context(), time.Now().Add(100*time.Millisecond))
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/items", func(ctx context.Context, req h.Request[item]) (h.Response[item], error) {
		time.Sleep(300 * time.Millisecond)
		if err := ctx.Err(); err != nil {
			return h.Response[item]{}, fmt.Errorf("request context ended after the body was read: %w", err)
		}
		return h.OK(req.Body()), nil
	})
	s := serve(t, r)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+"/items", strings.NewReader(`{"name":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func TestParallelRequestsKeepTheirBodies(t *testing.T) {
	r := newRouter(t, h.Config{SpillAt: 64})
	r.Post("/items", echo(nil))
	handler := handlerOf(t, r)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		wg.Go(func() {
			name := fmt.Sprintf("item-%d-%s", i, strings.Repeat("x", i*7))
			w := send(t, handler, call{method: "POST", target: "/items", body: `{"name":"` + name + `"}`, header: jsonHeader()})
			if want := `{"name":"` + name + `"}`; strings.TrimSpace(w.Body.String()) != want {
				errs <- fmt.Errorf("request %d got %q", i, w.Body)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestEmptyBodyPastDeadlineLeavesConnectionUsable(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(mw.Timeout(100 * time.Millisecond))
	r.Post("/upload", func(ctx context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		if _, err := io.Copy(io.Discard, req.Body().Reader()); err != nil {
			return h.Response[h.Empty]{}, err
		}
		if req.HTTP().URL.Query().Has("slow") {
			time.Sleep(300 * time.Millisecond)
		}
		return h.Response[h.Empty]{}, ctx.Err()
	}, h.Consumes("application/octet-stream"))
	s := serve(t, r)
	client := s.Client()
	post := func(target string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+target, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	post("/upload?slow")
	if got := post("/upload"); got != http.StatusNoContent {
		t.Fatalf("next request on the connection answered %d, want 204", got)
	}
}

func TestReplacedBodyPastDeadlineLeavesConnectionUsable(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			b, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(b))
			next.ServeHTTP(w, req)
		})
	}, mw.Timeout(100*time.Millisecond))
	r.Post("/upload", func(ctx context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		if _, err := io.Copy(io.Discard, req.Body().Reader()); err != nil {
			return h.Response[h.Empty]{}, err
		}
		if req.HTTP().URL.Query().Has("slow") {
			time.Sleep(300 * time.Millisecond)
		}
		return h.Response[h.Empty]{}, ctx.Err()
	}, h.Consumes("application/octet-stream"))
	s := serve(t, r)
	post := func(target string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+target, strings.NewReader("xyz"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	post("/upload?slow")
	if got := post("/upload"); got != http.StatusNoContent {
		t.Fatalf("next request on the connection answered %d, want 204", got)
	}
}

func TestUnreadStalledBodyIsBoundedByReadTimeout(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/ping", func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), nil
	})
	s := httptest.NewUnstartedServer(handlerOf(t, r))
	s.Config.ReadTimeout = 300 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /ping HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npartial")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	status, err := readStatusLine(conn)
	if err != nil || status != "HTTP/1.1 204 No Content" {
		t.Fatalf("status line %q, %v", status, err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("connection not closed by the server: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("connection held %v past a 300ms ReadTimeout", elapsed)
	}
}

func TestPanickingOnErrorReachesServerErrorLog(t *testing.T) {
	r := newRouter(t, h.Config{OnError: func(h.Failure) { panic("observer broke") }})
	r.Get("/x", failing(errors.New("boom")))
	var logged syncBuffer
	s := httptest.NewUnstartedServer(handlerOf(t, r))
	s.Config.ErrorLog = log.New(&logged, "", 0)
	s.Start()
	t.Cleanup(s.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("got status %d, want the connection closed", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logged.String(), "observer broke") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logged.String(), "observer broke") {
		t.Fatalf("server log %q", logged.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMaxBodyBoundsEmptyInputRoutes(t *testing.T) {
	var readErr error
	r := newRouter(t, h.Config{MaxBody: 4})
	r.Post("/raw", func(_ context.Context, req h.Request[h.Empty]) (h.Response[h.Empty], error) {
		_, readErr = io.ReadAll(req.HTTP().Body)
		return h.Response[h.Empty]{}, readErr
	})
	send(t, r, call{method: "POST", target: "/raw", body: "far too long"}).Expect(t, http.StatusRequestEntityTooLarge)
	if _, ok := errors.AsType[*h.RequestError](readErr); !ok {
		t.Fatalf("read error %v, want *h.RequestError", readErr)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	set []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.set = append(d.set, t)
	return nil
}

func TestBodyReadDeadlineFollowsTheContextWhileABodyIsRead(t *testing.T) {
	ahead := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name     string
		body     string
		deadline time.Time
		want     []time.Time
	}{
		{"body and a deadline ahead", `{"name":"a"}`, ahead, []time.Time{ahead, {}}},
		{"no body", "", ahead, nil},
		{"a deadline already passed", `{"name":"a"}`, time.Now().Add(-time.Minute), nil},
		{"no deadline", `{"name":"a"}`, time.Time{}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(t, h.Config{})
			r.Post("/items", echo(nil))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/items", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			if !c.deadline.IsZero() {
				ctx, cancel := context.WithDeadline(req.Context(), c.deadline)
				defer cancel()
				req = req.WithContext(ctx)
			}
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			handlerOf(t, r).ServeHTTP(w, req)
			if !slices.EqualFunc(w.set, c.want, time.Time.Equal) {
				t.Fatalf("read deadlines set %v, want %v", w.set, c.want)
			}
		})
	}
}
