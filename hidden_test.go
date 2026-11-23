package h_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/mw"
	"github.com/jacoelho/h/problem"
)

var errEndpoint = errors.New("endpoint failed")

type cancelKey struct{}

func cancelRequest(ctx context.Context) { ctx.Value(cancelKey{}).(context.CancelFunc)() }

func bigItems(after func(ctx context.Context) error) h.Endpoint[h.Empty, h.Seq[item]] {
	return func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(func(yield func(item, error) bool) {
			if !yield(item{strings.Repeat("x", 64)}, nil) {
				return
			}
			yield(item{}, after(ctx))
		})), nil
	}
}

func bigStream(after func(ctx context.Context) error) h.Endpoint[h.Empty, h.Stream] {
	return func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/plain", func(w *h.StreamWriter) error {
			if _, err := w.Write([]byte(strings.Repeat("x", 64))); err != nil {
				return err
			}
			return after(ctx)
		})), nil
	}
}

func TestHiddenReportsFailuresTheProblemDoesNotShow(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, SpillAt: 16,
		RequestErrors: problem.Func(func(error) (problem.Problem, bool) {
			return problem.Problem{Status: http.StatusInternalServerError}, true
		})})
	r.Post("/refused", echo(nil))
	r.Get("/unmapped", failing(errEndpoint))
	r.Get("/panics", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) { panic("boom") })
	r.Get("/seq-fails", bigItems(func(context.Context) error { return errEndpoint }))
	r.Get("/stream-fails", bigStream(func(context.Context) error { return errEndpoint }))
	r.Get("/seq-client-gone", bigItems(func(ctx context.Context) error { cancelRequest(ctx); return ctx.Err() }))
	r.Get("/stream-fails-after-client-gone", bigStream(func(ctx context.Context) error {
		cancelRequest(ctx)
		return errEndpoint
	}))
	r.Get("/panics-after-client-gone", bigStream(func(ctx context.Context) error {
		cancelRequest(ctx)
		panic("boom")
	}))
	r.Get("/seq-deadline", bigItems(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }), h.Use(mw.Timeout(10*time.Millisecond)))
	r.Get("/client-cancelled", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[item], error) {
		cancelRequest(ctx)
		return h.Response[item]{}, ctx.Err()
	})
	c := htest.New(t, r)

	for _, tc := range []struct {
		name, method, path string
		hidden             bool
	}{
		{"refusal mapper answer outside 4xx, sent as 400", "POST", "/refused", true},
		{"endpoint error after the response started, Seq", "GET", "/seq-fails", true},
		{"endpoint error after the response started, Stream", "GET", "/stream-fails", true},
		{"deadline cutting a started response", "GET", "/seq-deadline", true},
		{"client gone during a Seq", "GET", "/seq-client-gone", false},
		{"endpoint error after the response started and the client went away", "GET", "/stream-fails-after-client-gone", true},
		{"panic after the response started and the client went away", "GET", "/panics-after-client-gone", true},
		{"client cancelled before the answer", "GET", "/client-cancelled", false},
		{"unmapped error answered 500", "GET", "/unmapped", false},
		{"panic answered 500", "GET", "/panics", true},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		ctx = context.WithValue(ctx, cancelKey{}, cancel)
		before := len(log.All())
		c.Send(httptest.NewRequestWithContext(ctx, tc.method, tc.path, strings.NewReader("{")))
		cancel()
		all := log.All()
		if len(all) != before+1 {
			t.Errorf("%s: %d OnError calls, want 1", tc.name, len(all)-before)
			continue
		}
		if f := all[before]; f.Hidden() != tc.hidden {
			t.Errorf("%s: Hidden() = %v, want %v (status %d, err %v)", tc.name, f.Hidden(), tc.hidden, f.Problem.Status, f.Err)
		}
	}
}

type failingConn struct{ http.ResponseWriter }

func (failingConn) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }

func TestHiddenIsFalseWhenTheConnectionCannotCarryTheResponse(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, SpillAt: 16})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { next.ServeHTTP(failingConn{w}, req) })
	})
	r.Get("/stream", bigStream(func(context.Context) error { return nil }))
	r.Get("/seq", bigItems(func(context.Context) error { return errEndpoint }))
	r.Handle("GET", "/handle", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("partial"))
		return err
	})
	c := htest.New(t, r)
	for _, path := range []string{"/stream", "/seq", "/handle"} {
		before := len(log.All())
		c.Do("GET", path, nil)
		all := log.All()
		if len(all) != before+1 {
			t.Errorf("GET %s: %d OnError calls, want 1", path, len(all)-before)
			continue
		}
		if f := all[before]; f.Hidden() || !errors.Is(f.Err, h.ErrEncodeAfterCommit) {
			t.Errorf("GET %s: Hidden() = %v, err %v; want a failed started response, not hidden", path, f.Hidden(), f.Err)
		}
	}
}

// cutConn fails the response with err, as the server does when it cuts a
// response: on a write, on a flush, or, as an HTTP/2 stream reset does, by
// canceling the request's context while writes succeed and failing the next
// flush.
type cutConn struct {
	http.ResponseWriter
	cancel context.CancelFunc
	on     string
	err    error
}

func (c cutConn) Write(b []byte) (int, error) {
	switch c.on {
	case "write":
		c.cancel()
		return 0, c.err
	case "reset":
		c.cancel()
	}
	return c.ResponseWriter.Write(b)
}

func (c cutConn) FlushError() error {
	if c.on == "write" {
		return nil
	}
	c.cancel()
	return c.err
}

func TestHiddenTellsAResponseTheServerCutFromOneTheClientLeft(t *testing.T) {
	deadline := fmt.Errorf("write tcp: %w", os.ErrDeadlineExceeded)
	for _, tc := range []struct {
		name, on string
		err      error
		hidden   bool
	}{
		{"write deadline on a write", "write", deadline, true},
		{"write deadline on a flush", "flush", deadline, true},
		{"write deadline resetting the stream", "reset", deadline, true},
		{"body longer than its Content-Length", "write", http.ErrContentLength, true},
		{"http.TimeoutHandler deadline", "write", http.ErrHandlerTimeout, true},
		{"client gone on a write", "write", errors.New("client disconnected"), false},
		{"client gone on a flush", "flush", errors.New("client disconnected"), false},
		{"client gone resetting the stream", "reset", errors.New("client disconnected"), false},
	} {
		var log htest.Errors
		r := newRouter(t, h.Config{OnError: log.OnError, SpillAt: 16})
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx, cancel := context.WithCancel(req.Context())
				defer cancel()
				next.ServeHTTP(cutConn{w, cancel, tc.on, tc.err}, req.WithContext(ctx))
			})
		})
		r.Get("/stream", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Stream], error) {
			return h.OK(h.NewStream("text/plain", func(w *h.StreamWriter) error {
				if _, err := w.Write([]byte(strings.Repeat("x", 64))); err != nil {
					return err
				}
				if tc.on == "flush" {
					if err := w.Flush(); err != nil {
						return err
					}
				}
				<-ctx.Done()
				return ctx.Err()
			})), nil
		})
		htest.New(t, r).Do("GET", "/stream", nil)
		if f := log.Last(t); f.Hidden() != tc.hidden || !errors.Is(f.Err, h.ErrEncodeAfterCommit) {
			t.Errorf("%s: Hidden() = %v, err %v; want %v", tc.name, f.Hidden(), f.Err, tc.hidden)
		}
	}
}

func TestHiddenReportsAPanicInARefusalMapper(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError,
		RequestErrors: problem.Func(func(error) (problem.Problem, bool) { panic("mapper bug") })})
	r.Fallback()
	htest.New(t, r).Do("GET", "/missing", nil)
	if f := log.Last(t); !f.Hidden() || f.Problem.Status != http.StatusNotFound {
		t.Fatalf("Hidden() = %v, status %d, err %v; want a hidden panic answered 404", f.Hidden(), f.Problem.Status, f.Err)
	}
}

func TestAPanicInAnnotateIsReportedWithTheFailure(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError,
		Annotate: func(h.Failure) problem.Problem { panic("annotate bug") }})
	r.Fallback()
	htest.New(t, r).Do("GET", "/missing", nil)
	f := log.Last(t)
	if !errors.Is(f.Err, h.ErrNoRoute) || !errors.Is(f.Err, h.ErrPanic) || !f.Hidden() || f.Problem.Status != http.StatusNotFound {
		t.Fatalf("status %d, err %v, Hidden() = %v; want 404 reporting the missing route and the panic, hidden", f.Problem.Status, f.Err, f.Hidden())
	}
}

func TestHiddenReportsAProblemFormatFailure(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, ProblemFormats: []problem.Format{brokenFormat{}}})
	r.Fallback()
	htest.New(t, r).Do("GET", "/missing", nil).Expect(t, http.StatusNotFound)
	all := log.All()
	if len(all) != 2 || all[0].Hidden() || !all[1].Hidden() {
		t.Fatalf("OnError calls %d; want the 404 not hidden, then the format failure hidden", len(all))
	}
}

func TestHiddenReportsMultipartFilesLeftBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads the temporary directory from TMP and TEMP, and a read-only mode does not stop removing files")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, MaxMemory: 1})
	r.Post("/upload", func(_ context.Context, req h.Request[h.Form]) (h.Response[h.Empty], error) {
		if err := os.Chmod(dir, 0o500); err != nil {
			return h.Response[h.Empty]{}, err
		}
		return h.Response[h.Empty]{}, req.Body().Parse(func(p *h.FormParams) { p.File("doc") })
	})
	body, ct := multipartBody(t, nil, map[string]string{"doc": strings.Repeat("x", 64<<10)})
	send(t, r, call{method: "POST", target: "/upload", body: body, header: http.Header{"Content-Type": {ct}}}).Expect(t, http.StatusNoContent)
	f := log.Last(t)
	if f.Problem.Status != 0 || !f.Hidden() {
		t.Fatalf("OnError got status %d, Hidden() = %v, err %v; want the zero Problem, hidden", f.Problem.Status, f.Hidden(), f.Err)
	}
}

func TestZeroFailureIsNotHidden(t *testing.T) {
	if (h.Failure{}).Hidden() {
		t.Fatal("zero Failure reported hidden")
	}
	if !(h.Failure{Err: h.ErrEncodeAfterCommit}).Hidden() {
		t.Fatal("a Failure with no Request and a failed started response is not hidden")
	}
}
