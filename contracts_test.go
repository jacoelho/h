package h_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
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
	g := r.Group("/api", h.RequestErrors(problem.Is(h.ErrCrossOrigin, problem.Problem{Status: 403, Title: "Cross-site request refused"})))
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

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Listener.Addr().String())
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
	r.Handle("PROPFIND", "/dav", h.Raw(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMultiStatus) }))
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
	r := newRouter(t, h.Config{OnError: func(h.Failure) {
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

func TestPanickingFormEndpointLeavesNoTempFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	r := newRouter(t, h.Config{MaxMemory: 1})
	r.Post("/upload", func(context.Context, h.Request[h.Form]) (h.Response[h.Empty], error) { panic("endpoint broke") })

	var body bytes.Buffer
	mpw := multipart.NewWriter(&body)
	fw, err := mpw.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(bytes.Repeat([]byte("x"), 64<<10))
	_ = mpw.Close()
	htest.New(t, r).Do("POST", "/upload", body.Bytes(), htest.Header("Content-Type", mpw.FormDataContentType())).Expect(t, http.StatusInternalServerError)

	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestProblemKeepsFieldsSetOutsideTheRoute(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Encoding", "identity")
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/x", failing(errors.New("boom")))
	resp := htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError)
	if resp.Header().Get("Content-Encoding") != "identity" || resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("middleware fields dropped: %v", resp.Header())
	}
}

func TestSeqPipelineReadsWholeBodyWhileWriting(t *testing.T) {
	r := newRouter(t, h.Config{Codecs: ndjsonCodecs(t), SpillAt: 1})
	r.Post("/double", func(_ context.Context, req h.Request[h.Seq[int]]) (h.Response[h.Seq[int]], error) {
		return h.OK(h.Items(func(yield func(int, error) bool) {
			for n, err := range req.Body().All() {
				if !yield(2*n, err) {
					return
				}
			}
		})), nil
	})
	var in strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&in, "%d\n", i)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, serve(t, r).URL+"/double", strings.NewReader(in.String()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("after %d bytes: %v", len(out), err)
	}
	if lines := strings.Count(string(out), "\n"); lines != 2000 {
		t.Fatalf("%d lines, want 2000", lines)
	}
}

func TestCORSResponsesVaryOnOriginWithoutOne(t *testing.T) {
	r := newRouter(t, h.Config{TrustedOrigins: []string{"https://app.example.com"}})
	r.Get("/x", respond(h.OK(item{"a"})))
	if v := htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusOK).Header().Values("Vary"); !slices.Contains(strings.Split(strings.Join(v, ","), ","), "Origin") {
		t.Fatalf("Vary %q, want Origin", v)
	}
}

func TestStreamWriterClosesWhenStreamPanics(t *testing.T) {
	var kept *h.StreamWriter
	r := newRouter(t, h.Config{})
	r.Get("/s", func(context.Context, h.Request[h.Empty]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/plain", func(w *h.StreamWriter) error {
			kept = w
			panic("stream broke")
		})), nil
	})
	htest.New(t, r).Do("GET", "/s", nil).Expect(t, http.StatusInternalServerError)
	if _, err := kept.Write([]byte("late")); !errors.Is(err, h.ErrStreamClosed) {
		t.Fatalf("write after the panic: %v", err)
	}
}

func TestMalformedQueryKeepsTheValuesItParsed(t *testing.T) {
	var term string
	r := newRouter(t, h.Config{})
	r.Get("/s", func(_ context.Context, req h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), req.Parse(func(p *h.Params) { term = p.Query("q", h.String) })
	})
	resp := htest.New(t, r).Do("GET", "/s?q=tea&limit=%zz", nil).Expect(t, http.StatusBadRequest)
	if term != "tea" || strings.Contains(resp.Body.String(), `"name":"q"`) {
		t.Fatalf("q = %q; body %s", term, resp.Body)
	}
}

func TestStatusZeroIsInvalid(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Get("/x", respond(h.Status(0, item{"a"})))
	htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidResponse) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}
}

func TestNilFunctionsAreSetupErrors(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/f", h.Func[h.Empty, item](nil))
	r.Get("/b", h.Bind[h.Empty, string, item](nil, func(context.Context, string) (item, error) { return item{}, nil }))
	wantSetupError(t, r, `GET "/f": nil endpoint`)
	wantSetupError(t, r, `GET "/b": nil endpoint`)
}

func TestRawServesAHandlerByMethodValue(t *testing.T) {
	r := newRouter(t, h.Config{})
	files := http.FileServerFS(fstest.MapFS{"hello.txt": {Data: []byte("hi")}})
	r.Handle("GET", "/hello.txt", h.Raw(files.ServeHTTP))
	if got := htest.New(t, r).Do("GET", "/hello.txt", nil).Expect(t, http.StatusOK).Body.String(); got != "hi" {
		t.Fatalf("body %q", got)
	}
}

func TestRawNilIsASetupError(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Handle("GET", "/nil", h.Raw(nil))
	wantSetupError(t, r, `GET "/nil"`)
}

func TestBodyLimitIsNotOfferedToMalformedInputMappers(t *testing.T) {
	var log htest.Errors
	var offered []error
	r := newRouter(t, h.Config{OnError: log.OnError, Errors: problem.As(func(e *codec.DecodeError) problem.Problem {
		offered = append(offered, e)
		return problem.Problem{Status: http.StatusBadRequest, Title: "Malformed JSON"}
	})})
	r.Post("/one", h.Func(func(_ context.Context, in item) (item, error) { return in, nil }), h.MaxBody(8))
	r.Post("/many", func(_ context.Context, req h.Request[h.Seq[item]]) (h.Response[h.Empty], error) {
		for _, err := range req.Body().All() {
			if err != nil {
				return h.Response[h.Empty]{}, err
			}
		}
		return h.NoContent[h.Empty](), nil
	}, h.MaxBody(8))
	r.Post("/raw", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		var in item
		return h.NoContent[h.Empty](), codec.JSON{}.Decode(req.Body().Reader(), &in)
	}, h.Consumes("application/json"), h.MaxBody(8))
	c := htest.New(t, r)
	for path, body := range map[string]string{"/one": `{"name":"a long name"}`, "/many": `[{"name":"a long name"}]`, "/raw": `{"name":"a long name"}`} {
		c.Do("POST", path, body, htest.Header("Content-Type", "application/json")).Expect(t, http.StatusRequestEntityTooLarge)
		if err := log.Last(t).Err; errors.Is(err, h.ErrInvalidProblem) {
			t.Fatalf("POST %s: OnError got %v", path, err)
		}
	}
	if len(offered) != 0 {
		t.Fatalf("malformed-input mapper offered %v", offered)
	}
}

func TestFallbackProblemsCarryTitleAndMatchableCause(t *testing.T) {
	var log htest.Errors
	shape := problem.JSON("application/json", func(p problem.Problem) any {
		return map[string]any{"message": p.Title}
	})
	r := newRouter(t, h.Config{
		OnError:        log.OnError,
		ProblemFormats: []problem.Format{shape},
		RequestErrors:  problem.Is(h.ErrNoRoute, problem.Problem{Status: http.StatusNotFound, Title: "No such page"}),
	})
	r.Get("/x", respond(h.OK(item{"a"})))
	r.Fallback()
	c := htest.New(t, r)

	if got := c.Do("GET", "/nope", nil, htest.Header("Accept", "application/json")).Expect(t, http.StatusNotFound).Body.String(); got != `{"message":"No such page"}` {
		t.Fatalf("404 body %s", got)
	}
	if got := c.Do("POST", "/x", nil, htest.Header("Accept", "application/json")).Expect(t, http.StatusMethodNotAllowed).Body.String(); got != `{"message":"Method Not Allowed"}` {
		t.Fatalf("405 body %s", got)
	}
	if last := log.Last(t); !errors.Is(last.Err, h.ErrMethodNotAllowed) || last.Problem.Title != "Method Not Allowed" {
		t.Fatalf("OnError got %v, %+v", last.Err, last.Problem)
	}
}

func TestBodyWithoutContentTypeIsOctetStream(t *testing.T) {
	var got []string
	r := newRouter(t, h.Config{})
	r.Post("/any", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		data, err := io.ReadAll(req.Body().Reader())
		got = append(got, req.Body().ContentType()+":"+string(data))
		return h.NoContent[h.Empty](), err
	}, h.Consumes("*/*"))
	r.Post("/octets", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), nil
	}, h.Consumes("application/octet-stream"))
	r.Post("/json", h.Func(func(_ context.Context, in item) (item, error) { return in, nil }))
	c := htest.New(t, r)

	c.Do("POST", "/any", "abc").Expect(t, http.StatusNoContent)
	c.Do("POST", "/octets", "abc").Expect(t, http.StatusNoContent)
	c.Do("POST", "/json", `{"name":"a"}`).Expect(t, http.StatusUnsupportedMediaType)
	if !slices.Equal(got, []string{":abc"}) {
		t.Fatalf("endpoint saw %q, want the empty Content-Type kept", got)
	}
}

func TestDefaultCodecsCanRejectUnknownMembers(t *testing.T) {
	r := newRouter(t, h.Config{Codecs: codec.Default(codec.JSON{RejectUnknownFields: true})})
	r.Post("/x", h.Func(func(_ context.Context, in item) (item, error) { return in, nil }))
	c := htest.New(t, r)
	c.Do("POST", "/x", `{"nmae":"a"}`, htest.Header("Content-Type", "application/json")).Expect(t, http.StatusBadRequest) //nolint:misspell // "nmae" is deliberately not a member of item
	c.Do("POST", "/x", `{"name":"a"}`, htest.Header("Content-Type", "application/vnd.api+json")).Expect(t, http.StatusOK)
}

func TestLastProblemFormatAnswersUnmatchedAccept(t *testing.T) {
	fails := failing(errors.New("boom"))
	jsonOnly := newRouter(t, h.Config{ProblemFormats: []problem.Format{problem.RFC9457}})
	jsonOnly.Get("/x", fails)
	defaults := newRouter(t, h.Config{})
	defaults.Get("/x", fails)

	for _, tc := range []struct {
		name string
		r    *h.Router
		want string
	}{
		{"listed formats only", jsonOnly, "application/problem+json"},
		{"default keeps text last", defaults, "text/plain; charset=utf-8"},
	} {
		resp := htest.New(t, tc.r).Do("GET", "/x", nil, htest.Header("Accept", "application/xml"))
		if ct := resp.Header().Get("Content-Type"); resp.Code != http.StatusNotAcceptable || ct != tc.want {
			t.Fatalf("%s: %d %s, want 406 %s", tc.name, resp.Code, ct, tc.want)
		}
	}
}

func TestEmptyProblemFormatsAreRefused(t *testing.T) {
	if _, err := h.New(h.Config{ProblemFormats: []problem.Format{}}); err == nil || !strings.Contains(err.Error(), "Config.ProblemFormats") {
		t.Fatalf("New = %v, want an error naming Config.ProblemFormats", err)
	}
	r := newRouter(t, h.Config{})
	r.Get("/x", respond(h.OK(item{"a"})), h.ProblemFormats())
	wantSetupError(t, r, "ProblemFormats with no formats")
}

func TestRequestErrorsMapperSeesErrorJoinedWithParamsError(t *testing.T) {
	errBadOrder := errors.New("bad order")
	r := newRouter(t, h.Config{RequestErrors: problem.Is(errBadOrder, problem.Problem{Status: http.StatusBadRequest, Type: "https://example.com/bad-order"})})
	r.Get("/x", func(_ context.Context, req h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.Response[h.Empty]{}, errors.Join(errBadOrder, req.Parse(func(p *h.Params) { _ = p.Query("n", h.Int64) }))
	})
	p := htest.New(t, r).Do("GET", "/x?n=abc", nil).Expect(t, http.StatusBadRequest).Problem(t)
	if p.Type != "https://example.com/bad-order" {
		t.Fatalf("problem %+v, want the RequestErrors mapper's type", p)
	}
}

func TestContextEndingIsAnsweredByTheRouter(t *testing.T) {
	notFound := errors.New("not found")
	catchAll := problem.Func(func(error) (problem.Problem, bool) {
		return problem.Problem{Status: http.StatusInternalServerError}, true
	})
	r := newRouter(t, h.Config{Errors: problem.Map(problem.Is(notFound, problem.Problem{Status: http.StatusNotFound}), catchAll)})
	after := func(fail func(ctx context.Context) error) h.Endpoint[h.Empty, h.Empty] {
		return func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Empty], error) {
			<-ctx.Done()
			return h.Response[h.Empty]{}, fail(ctx)
		}
	}
	r.Get("/wraps", after(func(ctx context.Context) error { return fmt.Errorf("query: %w", ctx.Err()) }))
	r.Get("/unrelated", after(func(context.Context) error { return errors.New("driver: connection lost") }))
	r.Get("/notfound", after(func(context.Context) error { return notFound }))
	c := htest.New(t, r)

	for _, tc := range []struct {
		path   string
		cancel bool
		want   int
	}{
		{"/wraps", false, http.StatusServiceUnavailable},
		{"/wraps", true, 499},
		{"/unrelated", false, http.StatusInternalServerError},
		{"/notfound", false, http.StatusNotFound},
	} {
		ctx, stop := context.WithTimeout(context.Background(), time.Millisecond)
		if tc.cancel {
			stop()
		}
		if got := c.Send(httptest.NewRequestWithContext(ctx, "GET", tc.path, nil)).Code; got != tc.want {
			t.Errorf("GET %s (cancelled %v): %d, want %d", tc.path, tc.cancel, got, tc.want)
		}
		stop()
	}
}

type servings int

type fieldError struct{ field string }

func (e *fieldError) Error() string { return "invalid " + e.field }

func (s *servings) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if _, err := dec.ReadValue(); err != nil {
		return err
	}
	return &fieldError{field: "servings"}
}

func TestCatchAllErrorsMapperNeverSeesRefusals(t *testing.T) {
	var log htest.Errors
	var offered []error
	r := newRouter(t, h.Config{OnError: log.OnError, Errors: problem.Func(func(err error) (problem.Problem, bool) {
		offered = append(offered, err)
		return problem.Problem{Status: http.StatusInternalServerError}, true
	})})
	r.Post("/items", echo(nil))
	r.Fallback()
	c := htest.New(t, r)
	c.Do("GET", "/nope", nil).Expect(t, http.StatusNotFound)
	c.Do("POST", "/items", "x", htest.Header("Content-Type", "text/xml")).Expect(t, http.StatusUnsupportedMediaType)
	if len(offered) != 0 || errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("catch-all offered %v; OnError got %v", offered, log.Last(t).Err)
	}
}

func TestGroupRequestErrorsTurnAFieldErrorInsideABadBodyInto422(t *testing.T) {
	type recipe struct {
		Servings servings `json:"servings"`
	}
	r := newRouter(t, h.Config{})
	recipes := r.Group("/recipes", h.RequestErrors(problem.As(func(e *fieldError) problem.Problem {
		return problem.Problem{Status: http.StatusUnprocessableEntity, Extra: map[string]any{"field": e.field}}
	})))
	recipes.Post("/new", h.Func(func(_ context.Context, in recipe) (recipe, error) { return in, nil }))
	p := htest.New(t, r).Do("POST", "/recipes/new", `{"servings":2}`, htest.Header("Content-Type", "application/json")).Expect(t, http.StatusUnprocessableEntity).Problem(t)
	if p.Extra["field"] != "servings" {
		t.Fatalf("problem %+v", p)
	}
}

func TestAnnotateReachesEveryProblem(t *testing.T) {
	r := newRouter(t, h.Config{
		TrustedOrigins: []string{"https://app.example.com"},
		Annotate: func(f h.Failure) problem.Problem {
			id, _ := mw.RequestIDFrom(f.Request.Context())
			return f.Problem.With("request_id", id)
		},
	})
	r.Use(mw.RequestID("X-Request-Id"))
	r.Get("/fails", failing(errors.New("boom")))
	r.Get("/panics", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) { panic("broke") })
	r.Post("/items", echo(nil))
	r.Handle("GET", "/raw", func(http.ResponseWriter, *http.Request) error { return errors.New("raw broke") })
	r.Fallback()
	c := htest.New(t, r)
	id := htest.Header("X-Request-Id", "r1")

	for _, tc := range []struct {
		method, path string
		opts         []htest.RequestOption
		want         int
	}{
		{"GET", "/fails", nil, http.StatusInternalServerError},
		{"GET", "/panics", nil, http.StatusInternalServerError},
		{"GET", "/fails", []htest.RequestOption{htest.Header("Accept", "image/png, application/problem+json")}, http.StatusNotAcceptable},
		{"GET", "/raw", nil, http.StatusInternalServerError},
		{"GET", "/nope", nil, http.StatusNotFound},
		{"DELETE", "/fails", nil, http.StatusMethodNotAllowed},
		{"POST", "/items", []htest.RequestOption{htest.Header("Sec-Fetch-Site", "cross-site")}, http.StatusForbidden},
	} {
		p := c.Do(tc.method, tc.path, nil, append(tc.opts, id)...).Expect(t, tc.want).Problem(t)
		if p.Extra["request_id"] != "r1" {
			t.Errorf("%s %s: problem %+v lacks the request ID", tc.method, tc.path, p)
		}
	}
}

func TestAnnotateCannotChangeTheStatus(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{
		OnError:  log.OnError,
		Annotate: func(f h.Failure) problem.Problem { f.Problem.Status = http.StatusTeapot; return f.Problem },
	})
	r.Get("/x", failing(errors.New("boom")))
	htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v, want ErrInvalidProblem", log.Last(t).Err)
	}
}

func TestOnErrorCannotChangeTheResponse(t *testing.T) {
	r := newRouter(t, h.Config{
		Errors: problem.Func(func(error) (problem.Problem, bool) {
			return problem.Problem{Status: http.StatusInternalServerError, Extra: map[string]any{"code": "internal"}}, true
		}),
		OnError: func(f h.Failure) { f.Problem.Extra["leaked"] = true },
	})
	r.Get("/x", failing(errors.New("boom")))
	if p := htest.New(t, r).Do("GET", "/x", nil).Problem(t); p.Extra["leaked"] != nil {
		t.Fatalf("OnError changed the body: %+v", p.Extra)
	}
}

func TestEndpointCanReturnAProblem(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, Errors: problem.Func(func(error) (problem.Problem, bool) {
		return problem.Problem{Status: http.StatusInternalServerError}, true
	})})
	r.Get("/refuse", failing(fmt.Errorf("checking stock: %w", problem.Error(problem.Problem{Status: http.StatusConflict, Title: "Out of stock", Extra: map[string]any{"code": "out_of_stock"}}))))
	r.Get("/invalid", failing(problem.Error(problem.Problem{Status: http.StatusOK})))
	c := htest.New(t, r)

	p := c.Do("GET", "/refuse", nil).Expect(t, http.StatusConflict).Problem(t)
	if p.Title != "Out of stock" || p.Extra["code"] != "out_of_stock" {
		t.Fatalf("problem %+v", p)
	}
	c.Do("GET", "/invalid", nil).Expect(t, http.StatusInternalServerError)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v, want ErrInvalidProblem", log.Last(t).Err)
	}
}

func TestEmptyGroupPrefixAppliesOnlyOptions(t *testing.T) {
	errOutOfStock := errors.New("out of stock")
	r := newRouter(t, h.Config{})
	orders := r.Group("", h.Errors(problem.Is(errOutOfStock, problem.Problem{Status: http.StatusConflict})))
	orders.Get("/orders", failing(errOutOfStock))
	r.Get("/other", failing(errOutOfStock))
	c := htest.New(t, r)
	c.Do("GET", "/orders", nil).Expect(t, http.StatusConflict)
	c.Do("GET", "/other", nil).Expect(t, http.StatusInternalServerError)
}

type carriedServings int

func (s *carriedServings) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	v, err := dec.ReadValue()
	if err != nil {
		return err
	}
	status := http.StatusUnprocessableEntity
	if string(v) == "500" {
		status = http.StatusInternalServerError
	}
	return problem.Error(problem.Problem{Status: status, Title: "Bad servings"})
}

func TestProblemCarriedInsideARefusalIsSentWhen4xx(t *testing.T) {
	type order struct {
		Servings carriedServings `json:"servings"`
	}
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Post("/orders", h.Func(func(_ context.Context, in order) (order, error) { return in, nil }))
	c := htest.New(t, r)
	json := htest.Header("Content-Type", "application/json")

	if p := c.Do("POST", "/orders", `{"servings":2}`, json).Expect(t, http.StatusUnprocessableEntity).Problem(t); p.Title != "Bad servings" {
		t.Fatalf("problem %+v, want the carried one", p)
	}
	if p := c.Do("POST", "/orders", `{"servings":500}`, json).Expect(t, http.StatusBadRequest).Problem(t); p.Title == "Bad servings" {
		t.Fatalf("carried 500 escaped the refusal: %+v", p)
	}
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v, want ErrInvalidProblem", log.Last(t).Err)
	}
}

func TestARefusalMayBeAnsweredWith499(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, RequestErrors: problem.Is(h.ErrNoRoute,
		problem.Problem{Status: 499, Title: "Gone fishing", Header: http.Header{"Retry-After": {"5"}}})})
	r.Fallback()
	w := htest.New(t, r).Do("GET", "/nope", nil).Expect(t, 499)
	if errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After %q", got)
	}
	if !strings.Contains(w.Body.String(), "Gone fishing") {
		t.Fatalf("body %q", w.Body)
	}
}

func TestAllowIsSentOnlyWhileTheStatusIs405(t *testing.T) {
	r := newRouter(t, h.Config{RequestErrors: problem.Is(h.ErrMethodNotAllowed, problem.Problem{Status: http.StatusNotFound})})
	r.Get("/x", respond(h.OK(item{"a"})))
	r.Fallback()
	resp := htest.New(t, r).Do("DELETE", "/x", nil).Expect(t, http.StatusNotFound)
	if allow := resp.Header().Get("Allow"); allow != "" {
		t.Fatalf("Allow %q on a 404", allow)
	}
}

func TestRefusedBodiesWrapTheirCause(t *testing.T) {
	causes := problem.Map(
		problem.As(func(*http.MaxBytesError) problem.Problem { return problem.Problem{Status: 413, Title: "over MaxBody"} }),
		problem.Is(multipart.ErrMessageTooLarge, problem.Problem{Status: 413, Title: "form too large"}),
		problem.Is(os.ErrDeadlineExceeded, problem.Problem{Status: 408, Title: "too slow"}),
	)
	r := newRouter(t, h.Config{RequestErrors: causes, MaxBody: 64 << 10})
	r.Post("/typed", echo(nil))
	r.Post("/raw", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		_, err := io.ReadAll(req.Body().Reader())
		return h.NoContent[h.Empty](), err
	}, h.Consumes("*/*"))
	r.Post("/form", func(context.Context, h.Request[h.Form]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), nil
	}, h.MaxBody(1<<20))
	r.Post("/small-form", func(context.Context, h.Request[h.Form]) (h.Response[h.Empty], error) {
		return h.NoContent[h.Empty](), nil
	})

	bigFile, bigFileType := multipartBody(t, nil, map[string]string{"doc": strings.Repeat("x", 64<<10)})
	var parts strings.Builder
	form := multipart.NewWriter(&parts)
	for i := range 1001 {
		if err := form.WriteField(fmt.Sprint("f", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	oversized := `{"name":"` + strings.Repeat("x", 64<<10) + `"}`
	for _, c := range []struct {
		name, target, contentType, body, title string
		status                                 int
	}{
		{"typed body over MaxBody", "/typed", "application/json", oversized, "over MaxBody", 413},
		{"raw body over MaxBody", "/raw", "application/octet-stream", oversized, "over MaxBody", 413},
		{"multipart form over MaxBody", "/small-form", bigFileType, bigFile, "over MaxBody", 413},
		{"multipart form with too many parts", "/form", form.FormDataContentType(), parts.String(), "form too large", 413},
	} {
		w := send(t, r, call{method: "POST", target: c.target, body: c.body, header: http.Header{"Content-Type": {c.contentType}}})
		if p := w.Problem(t); p.Status != c.status || p.Title != c.title {
			t.Errorf("%s: problem %d %q, want %d %q", c.name, p.Status, p.Title, c.status, c.title)
		}
	}

	s := httptest.NewUnstartedServer(handlerOf(t, r))
	s.Config.ReadTimeout = 300 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)
	for _, target := range []string{"/typed", "/raw"} {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "POST "+target+" HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{\"name\":")
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := io.ReadAll(conn)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("slow body to %s: %v", target, err)
		}
		if !strings.HasPrefix(string(resp), "HTTP/1.1 408 ") || !strings.Contains(string(resp), `"title":"too slow"`) {
			t.Errorf("slow body to %s answered %q, want 408 titled \"too slow\"", target, resp)
		}
	}
}

func TestParserProblemAnswersAFailedParameter(t *testing.T) {
	noSuchOrder := problem.Problem{Status: http.StatusNotFound, Title: "Not Found", Detail: "No such order."}
	parseOrder := func(s string) (OrderID, error) {
		if !strings.HasPrefix(s, "o-") {
			return "", problem.Error(noSuchOrder)
		}
		return OrderID(s), nil
	}
	r := newRouter(t, h.Config{})
	r.Get("/orders/{id}", h.Bind(func(p *h.Params, _ h.Empty) OrderID { return p.Path("id", parseOrder) },
		func(_ context.Context, id OrderID) (Order, error) { return Order{ID: id}, nil }))
	p := htest.New(t, r).Do("GET", "/orders/42", nil).Expect(t, http.StatusNotFound).Problem(t)
	if p.Detail != "No such order." {
		t.Fatalf("problem %+v, want the parser's", p)
	}
}

func TestFirstFailedParameterWithAProblemWins(t *testing.T) {
	carrying := func(title string) func(string) (int64, error) {
		return func(string) (int64, error) {
			return 0, problem.Error(problem.Problem{Status: http.StatusUnprocessableEntity, Title: title})
		}
	}
	r := newRouter(t, h.Config{})
	r.Get("/x", func(_ context.Context, req h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.Response[h.Empty]{}, req.Parse(func(p *h.Params) {
			p.Query("plain", h.Int64)
			p.Query("first", carrying("first"))
			p.Query("second", carrying("second"))
		})
	})
	p := htest.New(t, r).Do("GET", "/x?plain=a&second=b&first=c", nil).Expect(t, http.StatusUnprocessableEntity).Problem(t)
	if p.Title != "first" {
		t.Fatalf("problem %+v, want the one from the first parameter asked for", p)
	}
}

func TestParserProblemOutside4xxKeepsTheParameterRefusal(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Get("/x", h.Bind(func(p *h.Params, _ h.Empty) int64 {
		return p.Query("n", func(string) (int64, error) {
			return 0, problem.Error(problem.Problem{Status: http.StatusServiceUnavailable})
		})
	}, func(context.Context, int64) (h.Empty, error) { return h.Empty{}, nil }))
	p := htest.New(t, r).Do("GET", "/x?n=1", nil).Expect(t, http.StatusBadRequest).Problem(t)
	if p.Extra["invalid_params"] == nil {
		t.Fatalf("problem %+v, want the parameter list", p)
	}
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v, want ErrInvalidProblem", log.Last(t).Err)
	}
}

func TestRequestErrorsMapperSeesTheParserError(t *testing.T) {
	r := newRouter(t, h.Config{RequestErrors: problem.As(func(*strconv.NumError) problem.Problem {
		return problem.Problem{Status: http.StatusBadRequest, Title: "Not a number"}
	})})
	r.Get("/x", h.Bind(func(p *h.Params, _ h.Empty) int64 { return p.Query("n", h.Int64) },
		func(context.Context, int64) (h.Empty, error) { return h.Empty{}, nil }))
	if p := htest.New(t, r).Do("GET", "/x?n=abc", nil).Expect(t, http.StatusBadRequest).Problem(t); p.Title != "Not a number" {
		t.Fatalf("problem %+v, want the mapper's", p)
	}
}
