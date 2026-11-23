package h_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

func TestHandle(t *testing.T) {
	errMissing := errors.New("photo missing")
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("photo"), 0o600); err != nil {
		t.Fatal(err)
	}
	photos := r.Group("/photos", h.Errors(problem.Is(errMissing, problem.Problem{Status: 404, Title: "No photo"})))
	photos.Handle("GET", "/{name}", func(w http.ResponseWriter, req *http.Request) error {
		name := req.PathValue("name")
		if name == "missing" {
			return errMissing
		}
		http.ServeFile(w, req, filepath.Join(dir, name))
		if name == "a.txt" && req.URL.Query().Has("fail") {
			return errors.New("failed after writing")
		}
		return nil
	})
	w := send(t, r, call{method: "GET", target: "/photos/a.txt"})
	w.Expect(t, http.StatusOK)
	if w.Body.String() != "photo" {
		t.Fatalf("body %q", w.Body)
	}
	w = send(t, r, call{method: "GET", target: "/photos/missing"})
	w.Expect(t, http.StatusNotFound)
	if title := w.Problem(t).Title; title != "No photo" {
		t.Fatalf("title %v", title)
	}
	if !send(t, r, call{method: "GET", target: "/photos/a.txt?fail"}).Aborted {
		t.Fatal("error after writing did not abort the connection")
	}
	if !errors.Is(log.Last(t).Err, h.ErrEncodeAfterCommit) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}
}

func TestFallback(t *testing.T) {
	r := newRouter(t, h.Config{})
	api := r.Group("/api")
	api.Get("/items", respond(h.OK(item{"a"})))
	api.Fallback()
	r.Handle("GET", "/other", h.Raw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "other")
	})))

	w := send(t, r, call{method: "GET", target: "/api/nope"})
	w.Expect(t, http.StatusNotFound)
	w.Problem(t)

	w = send(t, r, call{method: "DELETE", target: "/api/items"})
	w.Expect(t, http.StatusMethodNotAllowed)
	if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow %q", allow)
	}
	w.Problem(t)

	if w := send(t, r, call{method: "GET", target: "/other"}); w.Body.String() != "other" {
		t.Fatalf("/other answered %d %q", w.Code, w.Body)
	}
}

func TestGroupRootPath(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Group("/orders").Post("/", func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.Status(http.StatusCreated, h.Empty{}), nil
	})
	send(t, r, call{method: "POST", target: "/orders"}).Expect(t, http.StatusCreated)
	send(t, r, call{method: "POST", target: "/orders/x"}).Expect(t, http.StatusNotFound)
	send(t, r, call{method: "POST", target: "/orders/"}).Expect(t, http.StatusNotFound)
}

func TestTrailingSlashIsExact(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/docs/", respond(h.OK(item{"index"})))
	send(t, r, call{method: "GET", target: "/docs/"}).Expect(t, http.StatusOK)
	send(t, r, call{method: "GET", target: "/docs/x"}).Expect(t, http.StatusNotFound)
}

func TestBasePath(t *testing.T) {
	r := newRouter(t, h.Config{BasePath: "/svc/"})
	r.Get("/x", respond(h.OK(item{"a"})))
	if r.Mount() != "/svc/" {
		t.Fatalf("Mount %q", r.Mount())
	}
	outer := http.NewServeMux()
	outer.Handle(r.Mount(), handlerOf(t, r))
	send(t, outer, call{method: "GET", target: "/svc/x"}).Expect(t, http.StatusOK)
	for _, bad := range []string{"svc", "/a/../b", "/{x}"} {
		if _, err := h.New(h.Config{BasePath: bad}); err == nil {
			t.Fatalf("BasePath %q accepted", bad)
		}
	}
}

func TestRootMiddlewareSeesEverything(t *testing.T) {
	r := newRouter(t, h.Config{})
	type seen struct {
		status  int
		pattern string
	}
	var got []seen
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(rw, req)
			got = append(got, seen{rw.status, req.Pattern})
		})
	}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), markerKey{}, 1)))
		})
	})
	r.Get("/items/{id}", respond(h.OK(item{"a"})))
	send(t, r, call{method: "GET", target: "/items/1"})
	send(t, r, call{method: "GET", target: "/nope"})
	send(t, r, call{method: "POST", target: "/items/1"})
	want := []seen{{200, "GET /items/{id}"}, {404, ""}, {405, ""}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = 200
	}
	return s.ResponseWriter.Write(b)
}

func TestMiddlewareOrder(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(addHeader("X-Order", "root"))
	g := r.Group("/g", h.Use(addHeader("X-Order", "group-option")))
	g.Use(addHeader("X-Order", "group"))
	g.Get("/x", respond(h.OK(item{"a"})), h.Use(addHeader("X-Order", "route")))
	r.Get("/plain", respond(h.OK(item{"a"})))

	w := send(t, r, call{method: "GET", target: "/g/x"})
	if got := strings.Join(w.Header().Values("X-Order"), ","); got != "root,group-option,group,route" {
		t.Fatalf("order %s", got)
	}
	w = send(t, r, call{method: "GET", target: "/plain"})
	if got := strings.Join(w.Header().Values("X-Order"), ","); got != "root" {
		t.Fatalf("group middleware leaked: %s", got)
	}
}

type Unit struct{}

func TestSetupErrors(t *testing.T) {
	cases := []struct {
		name     string
		register func(r *h.Router)
		want     string
	}{
		{"named empty input", func(r *h.Router) {
			r.Post("/unit", func(context.Context, h.Request[Unit]) (h.Response[item], error) { return h.Response[item]{}, nil })
		}, "use h.Empty"},
		{"no encoder for output", func(r *h.Router) {
			r.Post("/chan", func(context.Context, h.Request[item]) (h.Response[chan int], error) {
				return h.Response[chan int]{}, nil
			})
		}, "POST /chan"},
		{"RawBody without Consumes", func(r *h.Router) {
			r.Post("/raw", func(context.Context, h.Request[h.RawBody]) (h.Response[h.Empty], error) {
				return h.Response[h.Empty]{}, nil
			})
		}, "must set Consumes"},
		{"route-only option on a group", func(r *h.Router) { r.Group("/g", h.InsecureAllowCrossOrigin()) }, "routes only"},
		{"bad group prefix", func(r *h.Router) { r.Group("g/").Get("/x", respond(h.OK(item{"a"}))) }, `"g/"`},
		{"path without slash", func(r *h.Router) { r.Get("x", respond(h.OK(item{"a"}))) }, `"x"`},
		{"duplicate route", func(r *h.Router) {
			r.Get("/x", respond(h.OK(item{"a"})))
			r.Get("/x", respond(h.OK(item{"b"})))
		}, "conflicts"},
		{"nil endpoint", func(r *h.Router) { r.Get("/x", h.Endpoint[h.Empty, item](nil)) }, "nil endpoint"},
		{"nil middleware", func(r *h.Router) { r.Use(nil) }, "nil middleware"},
		{"invalid mapper", func(r *h.Router) {
			r.Get("/x", respond(h.OK(item{"a"})), h.Errors(problem.Is(ErrOrderNotFound, problem.Problem{Status: 200})))
		}, `GET "/x": Errors: problem: invalid problem: status 200`},
		{"invalid group mapper", func(r *h.Router) {
			r.Group("/g", h.Errors(problem.As[*h.ParamsError](nil)))
		}, `group "/g"`},
		{"zero MaxBody", func(r *h.Router) { r.Get("/x", respond(h.OK(item{"a"})), h.MaxBody(0)) }, "MaxBody(0)"},
		{"malformed Consumes", func(r *h.Router) {
			r.Post("/raw", func(context.Context, h.Request[h.RawBody]) (h.Response[h.Empty], error) {
				return h.Response[h.Empty]{}, nil
			}, h.Consumes("nonsense"))
		}, "nonsense"},
		{"bad problem format", func(r *h.Router) {
			r.Get("/x", respond(h.OK(item{"a"})), h.ProblemFormats(problem.JSON("application/*", nil)))
		}, "application/*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(t, h.Config{})
			c.register(r)
			wantSetupError(t, r, c.want)
		})
	}
}

func TestSetupErrorsAreAllReported(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("x", respond(h.OK(item{"a"})))
	r.Use(nil)
	_, err := r.Handler()
	if err == nil || !strings.Contains(err.Error(), `"x"`) || !strings.Contains(err.Error(), "nil middleware") {
		t.Fatalf("Handler error %v, want both mistakes", err)
	}
}

func TestChangesAfterHandlerAreReportedNotApplied(t *testing.T) {
	cases := []struct {
		name   string
		change func(r *h.Router)
		probe  call
		check  func(t *testing.T, w *htest.Response)
		want   string
	}{
		{"route", func(r *h.Router) { r.Get("/b", respond(h.OK(item{"b"}))) },
			call{method: "GET", target: "/b"},
			func(t *testing.T, w *htest.Response) { w.Expect(t, http.StatusNotFound) },
			`"/b" registered after Handler`},
		{"root Use", func(r *h.Router) { r.Use(addHeader("X-Late", "1")) },
			call{method: "GET", target: "/a"},
			func(t *testing.T, w *htest.Response) {
				if w.Header().Get("X-Late") != "" {
					t.Fatal("late middleware ran")
				}
			},
			"Use after Handler"},
		{"group", func(r *h.Router) { r.Group("/g").Get("/x", respond(h.OK(item{"x"}))) },
			call{method: "GET", target: "/g/x"},
			func(t *testing.T, w *htest.Response) { w.Expect(t, http.StatusNotFound) },
			`Group "/g" created after Handler`},
		{"fallback", func(r *h.Router) { r.Fallback() },
			call{method: "GET", target: "/nope"},
			func(t *testing.T, w *htest.Response) {
				if w.Header().Get("Content-Type") == "application/problem+json" {
					t.Fatal("late fallback answered")
				}
			},
			"Fallback registered after Handler"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(t, h.Config{})
			r.Get("/a", respond(h.OK(item{"a"})))
			served := handlerOf(t, r)
			c.change(r)
			c.check(t, send(t, served, c.probe))
			wantSetupError(t, r, c.want)
		})
	}
}

func TestUseAppliesRegardlessOfOrder(t *testing.T) {
	r := newRouter(t, h.Config{})
	g := r.Group("/g")
	g.Get("/x", respond(h.OK(item{"a"})))
	g.Use(addHeader("X-Group", "1"))
	r.Use(addHeader("X-Root", "1"))
	w := send(t, r, call{method: "GET", target: "/g/x"})
	if w.Header().Get("X-Group") != "1" || w.Header().Get("X-Root") != "1" {
		t.Fatalf("header %v", w.Header())
	}
}

func TestSettingsResolveFromRoute(t *testing.T) {
	r := newRouter(t, h.Config{MaxBody: 1 << 20})
	g := r.Group("/g", h.MaxBody(16))
	g.Post("/small", echo(nil))
	g.Post("/big", echo(nil), h.MaxBody(1024))
	body := `{"name":"` + strings.Repeat("x", 32) + `"}`
	send(t, r, call{method: "POST", target: "/g/small", body: body, header: jsonHeader()}).Expect(t, http.StatusRequestEntityTooLarge)
	send(t, r, call{method: "POST", target: "/g/big", body: body, header: jsonHeader()}).Expect(t, http.StatusOK)
}

func TestFallbackCoversItsPrefix(t *testing.T) {
	r := newRouter(t, h.Config{})
	var called int
	api := r.Group("/api")
	api.Post("/", created(&called))
	api.Fallback()
	w := send(t, r, call{method: "GET", target: "/api"})
	w.Expect(t, http.StatusMethodNotAllowed)
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow %q", allow)
	}
	send(t, r, call{method: "POST", target: "/api"}).Expect(t, http.StatusCreated)
}

func TestRedirectsAreNotRoutes(t *testing.T) {
	r := newRouter(t, h.Config{TrustedOrigins: []string{"https://app.example"}})
	r.Get("/dir/", respond(h.OK(item{"a"})))
	r.Get("/items/{id}", respond(h.OK(item{"a"})))
	r.Fallback()
	send(t, r, call{method: "POST", target: "/dir"}).Expect(t, http.StatusNotFound)
	w := send(t, r, call{method: "OPTIONS", target: "http://api.example/items//1", header: http.Header{
		"Origin": {"https://app.example"}, "Access-Control-Request-Method": {"GET"},
	}})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("preflight for a redirecting path granted to %q", got)
	}
}

func TestHandleErrorAfterHijackIsNotWritten(t *testing.T) {
	for _, viaController := range []bool{true, false} {
		t.Run(fmt.Sprint("ResponseController=", viaController), func(t *testing.T) {
			var log htest.Errors
			r := newRouter(t, h.Config{OnError: log.OnError})
			r.Handle("GET", "/ws", func(w http.ResponseWriter, _ *http.Request) error {
				var conn net.Conn
				var brw *bufio.ReadWriter
				var err error
				if viaController {
					conn, brw, err = http.NewResponseController(w).Hijack()
				} else {
					conn, brw, err = w.(http.Hijacker).Hijack()
				}
				if err != nil {
					return err
				}
				_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: close\r\n\r\n")
				_ = brw.Flush()
				_ = conn.Close()
				return errors.New("socket failed after hijack")
			})
			s := serve(t, r)
			resp, err := http.Get(s.URL + "/ws")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status %d", resp.StatusCode)
			}
			deadline := time.Now().Add(2 * time.Second)
			for len(log.All()) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !errors.Is(log.Last(t).Err, h.ErrEncodeAfterCommit) {
				t.Fatalf("OnError got %v, want ErrEncodeAfterCommit", log.Last(t).Err)
			}
		})
	}
}

func TestHandleErrorAfterFlush(t *testing.T) {
	cases := []struct {
		name       string
		writer     func(http.ResponseWriter) http.ResponseWriter
		flush      func(http.ResponseWriter)
		wantStatus int
		wantErr    error
	}{
		{"Flusher then error aborts", nil,
			func(w http.ResponseWriter) { w.(http.Flusher).Flush() }, 0, h.ErrEncodeAfterCommit},
		{"failed flush then error writes a problem",
			func(w http.ResponseWriter) http.ResponseWriter {
				return failingFlusher{w.(*httptest.ResponseRecorder)}
			},
			func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }, http.StatusInternalServerError, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log htest.Errors
			r := newRouter(t, h.Config{OnError: log.OnError})
			r.Handle("GET", "/f", func(w http.ResponseWriter, _ *http.Request) error {
				c.flush(w)
				return errors.New("failed after flush")
			})
			var w http.ResponseWriter = httptest.NewRecorder()
			rec := w.(*httptest.ResponseRecorder)
			if c.writer != nil {
				w = c.writer(w)
			}
			aborted := func() (aborted bool) {
				defer func() {
					if v := recover(); v != nil {
						if v != http.ErrAbortHandler {
							panic(v)
						}
						aborted = true
					}
				}()
				handlerOf(t, r).ServeHTTP(w, httptest.NewRequest("GET", "/f", nil))
				return false
			}()
			if c.wantErr != nil {
				if !aborted || !errors.Is(log.Last(t).Err, c.wantErr) {
					t.Fatalf("aborted=%v OnError %v, want abort and %v", aborted, log.Last(t).Err, c.wantErr)
				}
				return
			}
			if aborted || rec.Code != c.wantStatus || rec.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("aborted=%v status %d %q, want a %d problem", aborted, rec.Code, rec.Header().Get("Content-Type"), c.wantStatus)
			}
		})
	}
}

func TestHandleErrorAfterEarlyHintsIsAProblem(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Handle("GET", "/hints", func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		return errors.New("failed after hints")
	})
	resp, err := http.Get(serve(t, r).URL + "/hints")
	if err != nil {
		t.Fatalf("connection aborted: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d %q, want a 500 problem", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
