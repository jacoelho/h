package h_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/mw"
	"github.com/jacoelho/h/problem"
)

func TestCrossOriginExemptionCoversOnlyItsRoute(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/items/{id}", h.Func(func(context.Context, item) (item, error) { return item{"hook"}, nil }), h.InsecureAllowCrossOrigin())
	r.Post("/items/new", h.Func(func(context.Context, item) (item, error) { return item{"new"}, nil }))
	c := htest.New(t, r)
	crossSite := htest.Header("Sec-Fetch-Site", "cross-site")

	c.Do("POST", "/items/7", item{"x"}, crossSite).Expect(t, http.StatusOK)
	c.Do("POST", "/items/new", item{"x"}, crossSite).Expect(t, http.StatusForbidden).Problem(t)
}

func TestCrossOriginRefusalUsesTheRoutesMappers(t *testing.T) {
	r := newRouter(t, h.Config{})
	g := r.Group("/api", h.Errors(problem.Is(h.ErrCrossOrigin, problem.Problem{Status: 403, Title: "Cross-site request refused"})))
	g.Post("/x", h.Func(func(context.Context, item) (item, error) { return item{}, nil }))
	p := htest.New(t, r).Do("POST", "/api/x", item{"x"}, htest.Header("Sec-Fetch-Site", "cross-site")).Expect(t, http.StatusForbidden).Problem(t)
	if p.Title != "Cross-site request refused" {
		t.Fatalf("title %q, want the group mapper's", p.Title)
	}
}

func TestServerReadTimeoutBoundsBodyPastContextDeadline(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(mw.Timeout(3 * time.Second))
	r.Post("/raw", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		_, err := io.ReadAll(req.Body().Reader())
		return h.NoContent[h.Empty](), err
	}, h.Consumes("application/octet-stream"))
	s := httptest.NewUnstartedServer(handlerOf(t, r))
	s.Config.ReadTimeout = 300 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)

	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /raw HTTP/1.1\r\nHost: x\r\nContent-Type: application/octet-stream\r\nContent-Length: 100\r\n\r\npartial")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if status, err := readStatusLine(conn); err != nil || status != "HTTP/1.1 408 Request Timeout" {
		t.Fatalf("status line %q, %v", status, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("408 after %v; the 300ms ReadTimeout was lifted", elapsed)
	}
}

type noFlushWriter struct{ http.ResponseWriter }

func TestNDJSONThroughNonFlushingMiddlewareIsDelivered(t *testing.T) {
	r := newRouter(t, h.Config{Codecs: ndjsonCodecs(t)})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { next.ServeHTTP(noFlushWriter{w}, req) })
	})
	r.Get("/items", func(context.Context, h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(func(yield func(item, error) bool) {
			_ = yield(item{"a"}, nil) && yield(item{"b"}, nil)
		})), nil
	})
	got := htest.New(t, r).Do("GET", "/items", nil, htest.Header("Accept", "application/x-ndjson")).Expect(t, http.StatusOK).Body.String()
	if got != "{\"name\":\"a\"}\n{\"name\":\"b\"}\n" {
		t.Fatalf("body %q", got)
	}
}

func TestRequestErrorOutsideErrorStatusesIsInvalid(t *testing.T) {
	for _, status := range []int{0, 200, 302, 600} {
		var log htest.Errors
		r := newRouter(t, h.Config{OnError: log.OnError})
		r.Get("/x", failing(&h.RequestError{Status: status, Err: errors.New("bad")}))
		htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError).Problem(t)
		if all := log.All(); len(all) != 1 || !errors.Is(all[0].Err, h.ErrInvalidProblem) {
			t.Fatalf("status %d: OnError got %+v, want one ErrInvalidProblem", status, all)
		}
	}
}

func TestProblemDropsTheFailedBodysRepresentationFields(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Handle("GET", "/f", func(w http.ResponseWriter, _ *http.Request) error {
		for k, v := range map[string]string{"Content-Encoding": "gzip", "ETag": `"v1"`, "Last-Modified": "Mon, 02 Jan 2006 15:04:05 GMT", "Cache-Control": "max-age=3600"} {
			w.Header().Set(k, v)
		}
		return errors.New("failed before writing")
	})
	resp := htest.New(t, r).Do("GET", "/f", nil).Expect(t, http.StatusInternalServerError)
	for _, k := range []string{"Content-Encoding", "ETag", "Last-Modified", "Cache-Control"} {
		if v := resp.Header().Get(k); v != "" {
			t.Errorf("problem kept %s: %q", k, v)
		}
	}
}

func TestFormRouteHonoursConsumes(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/upload", func(context.Context, h.Request[h.Form]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), nil
	}, h.Consumes("multipart/form-data"))
	htest.New(t, r).Do("POST", "/upload", url.Values{"a": {"b"}}.Encode(),
		htest.Header("Content-Type", "application/x-www-form-urlencoded")).Expect(t, http.StatusUnsupportedMediaType)
}

func TestReadKeepsInstanceInExtra(t *testing.T) {
	p, err := problem.Read(strings.NewReader(`{"status":404,"instance":"/orders/1","code":"nope"}`))
	if err != nil || p.Extra["instance"] != "/orders/1" || p.Extra["code"] != "nope" {
		t.Fatalf("Read = %+v, %v", p, err)
	}
}

func TestResponseCannotSetCORSFields(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Get("/x", respond(h.OK(item{"a"}).WithHeader("Access-Control-Allow-Origin", "*")))
	htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidResponse) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}
}

func TestFallbackAllowsEveryRegisteredMethod(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Handle("PROPFIND", "/dav", h.Raw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(207) })))
	r.Fallback()
	resp := htest.New(t, r).Do("GET", "/dav", nil).Expect(t, http.StatusMethodNotAllowed)
	if allow := resp.Header().Get("Allow"); allow != "PROPFIND" {
		t.Fatalf("Allow %q", allow)
	}
}

func TestEmptyRemainderWildcardReachesParser(t *testing.T) {
	var got *string
	r := newRouter(t, h.Config{})
	r.Get("/files/{rest...}", func(_ context.Context, req h.Request[h.Empty]) (h.Response[h.Empty], error) {
		var rest string
		err := req.Parse(func(p *h.Params) { rest = p.Path("rest", h.String) })
		got = &rest
		return h.NoContent[h.Empty](), err
	})
	htest.New(t, r).Do("GET", "/files/", nil).Expect(t, http.StatusNoContent)
	if got == nil || *got != "" {
		t.Fatalf("rest = %v", got)
	}
}

func TestNotModifiedCarriesVaryWhenNegotiated(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/label", respond(h.NotModified[string]()))
	if v := htest.New(t, r).Do("GET", "/label", nil).Expect(t, http.StatusNotModified).Header().Get("Vary"); !strings.Contains(v, "Accept") {
		t.Fatalf("Vary %q, want Accept", v)
	}
}

func TestPanickingOnErrorIsCalledOnce(t *testing.T) {
	var calls atomic.Int32
	r := newRouter(t, h.Config{OnError: func(context.Context, *http.Request, error, problem.Problem) {
		calls.Add(1)
		panic("observer broke")
	}})
	r.Get("/x", failing(errors.New("boom")))
	func() {
		defer func() { _ = recover() }()
		htest.New(t, r).Do("GET", "/x", nil)
	}()
	if n := calls.Load(); n != 1 {
		t.Fatalf("OnError called %d times", n)
	}
}

func TestCORSMaxAgeBelowOneSecondIsRefused(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, 500 * time.Millisecond} {
		if _, err := h.New(h.Config{TrustedOrigins: []string{"https://app.example"}, CORS: h.CORS{MaxAge: d}}); err == nil {
			t.Errorf("MaxAge %v accepted", d)
		}
	}
}
