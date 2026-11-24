package h_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

func respond[T any](resp h.Response[T]) h.Endpoint[h.Empty, T] {
	return func(context.Context, h.Request[h.Empty]) (h.Response[T], error) { return resp, nil }
}

func TestBodilessResponses(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/none", respond(h.NoContent[item]()))
	r.Get("/same", respond(h.NotModified[item]().WithHeader("ETag", `"v1"`)))
	r.Delete("/empty", respond(h.Response[h.Empty]{}))
	r.Post("/accepted", func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.Accepted(h.Empty{}), nil
	})
	cases := []struct {
		method, target string
		status         int
	}{
		{"GET", "/none", 204},
		{"GET", "/same", 304},
		{"DELETE", "/empty", 204},
		{"POST", "/accepted", 202},
	}
	for _, c := range cases {
		w := send(t, r, call{method: c.method, target: c.target})
		w.Expect(t, c.status)
		if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
			t.Fatalf("%s: body %q, Content-Type %q", c.target, w.Body, w.Header().Get("Content-Type"))
		}
	}
	if w := send(t, r, call{method: "GET", target: "/same"}); w.Header().Get("ETag") != `"v1"` {
		t.Fatal("304 lost its ETag")
	}
}

func TestStatus599IsAValidResponse(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", respond(h.Status(599, item{"a"})))
	send(t, r, call{method: "GET", target: "/x"}).Expect(t, 599)
}

func TestInvalidResponses(t *testing.T) {
	cases := map[string]h.Response[item]{
		"204 with body":        h.Status(204, item{"a"}),
		"1xx":                  h.Status(103, item{"a"}),
		"600":                  h.Status(600, item{"a"}),
		"Content-Length":       h.OK(item{"a"}).WithHeader("Content-Length", "3"),
		"Content-Type":         h.OK(item{"a"}).WithHeader("content-type", "text/html"),
		"bodiless 304 headers": h.NotModified[item]().WithHeader("Content-Type", "x/y"),
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			var log htest.Errors
			r := newRouter(t, h.Config{OnError: log.OnError})
			r.Get("/x", respond(resp))
			send(t, r, call{method: "GET", target: "/x"}).Expect(t, http.StatusInternalServerError)
			if !errors.Is(log.Last(t).Err, h.ErrInvalidResponse) {
				t.Fatalf("OnError got %v", log.Last(t).Err)
			}
		})
	}
}

func TestCreatedAndHeaderOverride(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(addHeader("Cache-Control", "no-store"))
	r.Post("/items", func(_ context.Context, req h.Request[item]) (h.Response[item], error) {
		return h.Created("/items/1", req.Body()).WithHeader("Cache-Control", "max-age=60").WithHeader("Vary", "Authorization"), nil
	})
	w := send(t, r, call{method: "POST", target: "/items", body: `{"name":"a"}`, header: jsonHeader()})
	w.Expect(t, http.StatusCreated)
	if got := w.Header().Get("Location"); got != "/items/1" {
		t.Fatalf("Location %q", got)
	}
	if got := w.Header().Values("Cache-Control"); !reflect.DeepEqual(got, []string{"max-age=60"}) {
		t.Fatalf("Cache-Control %q", got)
	}
	if got := w.Header().Values("Vary"); !reflect.DeepEqual(got, []string{"Authorization"}) {
		t.Fatalf("Vary %q", got)
	}
}

func TestWithHeaderDoesNotAlias(t *testing.T) {
	base := h.OK(item{"a"}).WithHeader("X-A", "1")
	one := base.WithHeader("X-B", "1")
	two := base.WithHeader("X-B", "2")
	if one.Header().Get("X-B") != "1" || two.Header().Get("X-B") != "2" || base.Header().Get("X-B") != "" {
		t.Fatalf("copies share storage: %v %v %v", base.Header(), one.Header(), two.Header())
	}
}

func TestZeroResponseStatus(t *testing.T) {
	if got := (h.Response[item]{}).Status(); got != 200 {
		t.Fatalf("zero Response[item] status %d", got)
	}
	if got := (h.Response[h.Empty]{}).Status(); got != 204 {
		t.Fatalf("zero Response[Empty] status %d", got)
	}
}

type failingEncoder struct{}

func (failingEncoder) Encode(w io.Writer, _ any) error {
	_, _ = io.WriteString(w, `{"partial":`)
	return errors.New("encoder broke")
}

func (failingEncoder) CanEncode(reflect.Type) bool { return true }

func TestEncoderFailureBeforeCommitIsProblem(t *testing.T) {
	codecs := codec.NewSet()
	if err := codecs.Encodes(failingEncoder{}, "application/json"); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t, h.Config{Codecs: codecs})
	r.Get("/x", respond(h.OK(item{"a"})))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusInternalServerError)
	w.Problem(t)
}

func TestEndpointsCallableWithoutRouter(t *testing.T) {
	ctx := context.Background()
	req := httptest.NewRequest("POST", "/signup/7?q=x", nil)
	req.SetPathValue("id", "7")
	var got struct {
		name string
		id   int64
	}
	formEndpoint := func(_ context.Context, r h.Request[h.Form]) (h.Response[h.Empty], error) {
		return h.Response[h.Empty]{}, r.Body().Parse(func(p *h.FormParams) {
			got.name = p.Value("name", h.String)
			got.id = p.Path("id", h.Int64)
		})
	}
	if _, err := formEndpoint(ctx, h.NewFormRequest(req, url.Values{"name": {"ana"}}, nil)); err != nil || got.name != "ana" || got.id != 7 {
		t.Fatalf("form endpoint: %+v, %v", got, err)
	}

	raw := h.NewRawBody("image/png", strings.NewReader("PNG"))
	if b, _ := io.ReadAll(raw.Reader()); raw.ContentType() != "image/png" || string(b) != "PNG" {
		t.Fatalf("raw body %q %q", raw.ContentType(), b)
	}

	s := h.NewStream("text/csv", func(w *h.StreamWriter) error {
		if _, err := io.WriteString(w, "a,b\n"); err != nil {
			return err
		}
		return w.Flush()
	})
	var buf strings.Builder
	n, err := s.WriteTo(&buf)
	if err != nil || n != 4 || buf.String() != "a,b\n" || s.ContentType() != "text/csv" {
		t.Fatalf("stream: %d %q %q %v", n, buf.String(), s.ContentType(), err)
	}
	if _, err := (h.Stream{}).WriteTo(&buf); !errors.Is(err, h.ErrInvalidResponse) {
		t.Fatalf("zero stream: %v", err)
	}
}

func TestWithHeaderRepeatedReplacesMiddlewareValuesWithAll(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Use(addHeader("Link", "<m>"))
	r.Get("/x", respond(h.OK(item{"a"}).WithHeader("Link", "<a>").WithHeader("Link", "<b>")))
	w := send(t, r, call{method: "GET", target: "/x"})
	if got := w.Header().Values("Link"); !reflect.DeepEqual(got, []string{"<a>", "<b>"}) {
		t.Fatalf("Link %q, want <a> and <b>", got)
	}
}

func TestVaryNamesEachFieldOnce(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", respond(h.OK("a").WithHeader("Vary", "accept, , Authorization, authorization")))
	w := send(t, r, call{method: "GET", target: "/x"})
	if got := w.Header().Values("Vary"); !reflect.DeepEqual(got, []string{"Accept", "Authorization"}) {
		t.Fatalf("Vary %q, want Accept and Authorization once each", got)
	}
}

func TestRequestErrorStatusMayBe599(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) {
		return h.Response[item]{}, &h.RequestError{Status: 599, Err: errEndpoint}
	})
	send(t, r, call{method: "GET", target: "/x"}).Expect(t, 599)
}

type partialFormat struct{}

func (partialFormat) MediaType() string { return "application/x-partial" }

func (partialFormat) Write(w io.Writer, _ problem.Problem) error {
	_, _ = io.WriteString(w, "partial")
	return errors.New("format failed")
}

func TestAFailedProblemFormatLeavesOnlyTheText(t *testing.T) {
	r := newRouter(t, h.Config{ProblemFormats: []problem.Format{partialFormat{}, problem.Text}})
	r.Fallback()
	w := send(t, r, call{method: "GET", target: "/nope", header: http.Header{"Accept": {"application/x-partial"}}})
	w.Expect(t, http.StatusNotFound)
	if w.Body.String() != "Not Found\n" {
		t.Fatalf("body %q, want only the text problem", w.Body)
	}
}

type silent struct{}

func (silent) Encode(io.Writer, any) error { return nil }
func (silent) CanEncode(reflect.Type) bool { return true }

func TestAnEmptyEncodedBodyHasLengthZero(t *testing.T) {
	codecs := codec.NewSet()
	if err := codecs.Encodes(silent{}, "application/x-silent"); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t, h.Config{Codecs: codecs})
	r.Get("/x", respond(h.OK(item{"a"})))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusOK)
	if got := w.Header().Get("Content-Length"); got != "0" {
		t.Fatalf("Content-Length %q, want 0", got)
	}
}
