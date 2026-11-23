package h_test

import (
	"context"
	"github.com/jacoelho/h/htest"
	"net/http"
	"strings"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
)

type item struct {
	Name string `json:"name"`
}

func echo(called *bool) h.Endpoint[item, item] {
	return func(_ context.Context, r h.Request[item]) (h.Response[item], error) {
		if called != nil {
			*called = true
		}
		return h.OK(r.Body()), nil
	}
}

func TestStructOutputRefusesTextAccept(t *testing.T) {
	r := newRouter(t, h.Config{})
	var called bool
	r.Post("/items", echo(&called))
	w := send(t, r, call{method: "POST", target: "/items", body: `{"name":"a"}`, header: http.Header{
		"Content-Type": {"application/json"}, "Accept": {"text/plain"},
	}})
	w.Expect(t, http.StatusNotAcceptable)
	if called {
		t.Fatal("endpoint called for an unacceptable request")
	}
}

func TestVaryAcceptOnlyWhenNegotiated(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/one", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) { return h.OK(item{"a"}), nil })
	r.Get("/two", func(context.Context, h.Request[h.Empty]) (h.Response[string], error) { return h.OK("a"), nil })

	w := send(t, r, call{method: "GET", target: "/one"})
	w.Expect(t, http.StatusOK)
	if v := w.Header().Values("Vary"); len(v) != 0 {
		t.Fatalf("Vary %q with a single encoder", v)
	}
	w = send(t, r, call{method: "GET", target: "/two", header: http.Header{"Accept": {"text/plain"}}})
	w.Expect(t, http.StatusOK)
	if v := w.Header().Get("Vary"); v != "Accept" {
		t.Fatalf("Vary %q with two encoders", v)
	}
	if ct, body := w.Header().Get("Content-Type"), w.Body.String(); ct != "text/plain; charset=utf-8" || body != "a" {
		t.Fatalf("text answer %q %q", ct, body)
	}
}

func TestRequestBodyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		status int
	}{
		{"unsupported content type", http.Header{"Content-Type": {"application/xml"}}, `<a/>`, http.StatusUnsupportedMediaType},
		{"missing content type", nil, `{"name":"a"}`, http.StatusUnsupportedMediaType},
		{"empty body", jsonHeader(), ``, http.StatusBadRequest},
		{"trailing data", jsonHeader(), `{"name":"a"} {}`, http.StatusBadRequest},
		{"over MaxBody", jsonHeader(), `{"name":"` + strings.Repeat("x", 64) + `"}`, http.StatusRequestEntityTooLarge},
		{"+json media type", http.Header{"Content-Type": {"application/vnd.item+json"}}, `{"name":"a"}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(t, h.Config{MaxBody: 32})
			var called bool
			r.Post("/items", echo(&called))
			w := send(t, r, call{method: "POST", target: "/items", body: c.body, header: c.header})
			w.Expect(t, c.status)
			if c.status != http.StatusOK && called {
				t.Fatal("endpoint called")
			}
			if c.status != http.StatusOK {
				if p := w.Problem(t); p.Status != c.status {
					t.Fatalf("problem %v", p)
				}
			}
		})
	}
}

func TestProducesNarrowsEncoders(t *testing.T) {
	codecs := codec.Default(codec.JSON{})
	if err := codecs.Encodes(codec.NDJSON{}, "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t, h.Config{Codecs: codecs})
	r.Get("/feed", func(context.Context, h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(func(yield func(item, error) bool) { yield(item{"a"}, nil) })), nil
	}, h.Produces("application/x-ndjson"))
	w := send(t, r, call{method: "GET", target: "/feed", header: http.Header{"Accept": {"application/json"}}})
	w.Expect(t, http.StatusNotAcceptable)
	w = send(t, r, call{method: "GET", target: "/feed"})
	w.Expect(t, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type %q", ct)
	}
}

func TestConsumesNarrowsDecoders(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/items", echo(nil), h.Consumes("application/vnd.api+json"))
	for ct, status := range map[string]int{
		"application/vnd.api+json": http.StatusOK,
		"application/other+json":   http.StatusUnsupportedMediaType,
		"application/json":         http.StatusUnsupportedMediaType,
	} {
		w := send(t, r, call{method: "POST", target: "/items", body: `{"name":"a"}`, header: http.Header{"Content-Type": {ct}}})
		if w.Code != status {
			t.Fatalf("%s answered %d, want %d", ct, w.Code, status)
		}
	}
}

type shoutOut string

func (s shoutOut) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(string(s))), nil }

type wordIn struct{ word string }

func (w *wordIn) UnmarshalText(b []byte) error { w.word = string(b); return nil }

func TestOneDirectionalTextTypes(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/shout", func(context.Context, h.Request[h.Empty]) (h.Response[shoutOut], error) {
		return h.OK(shoutOut("hi")), nil
	})
	r.Post("/word", h.Func(func(_ context.Context, in wordIn) (string, error) { return in.word, nil }))
	c := htest.New(t, r)

	if got := c.Do("GET", "/shout", nil, htest.Header("Accept", "text/plain")).Expect(t, http.StatusOK).Body.String(); got != "HI" {
		t.Fatalf("marshal-only output %q", got)
	}
	if got := c.Do("POST", "/word", "tea", htest.Header("Content-Type", "text/plain"), htest.Header("Accept", "text/plain")).Expect(t, http.StatusOK).Body.String(); got != "tea" {
		t.Fatalf("unmarshal-only input %q", got)
	}
}

func ldOnly(t *testing.T) *codec.Set {
	t.Helper()
	s := codec.NewSet()
	if err := s.Encodes(codec.JSON{}, "application/ld+json"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodecsClosestScopeWins(t *testing.T) {
	r := newRouter(t, h.Config{})
	ld := r.Group("/ld", h.Codecs(ldOnly(t)))
	ld.Get("/inherited", respond(h.OK(item{"a"})))
	ld.Get("/own", respond(h.OK(item{"b"})), h.Codecs(codec.Default(codec.JSON{})))
	r.Get("/plain", respond(h.OK(item{"c"})))
	for _, c := range []struct {
		target, accept string
		status         int
		contentType    string
	}{
		{"/ld/inherited", "", http.StatusOK, "application/ld+json"},
		{"/ld/inherited", "application/json", http.StatusNotAcceptable, "application/problem+json"},
		{"/ld/own", "application/ld+json", http.StatusNotAcceptable, "text/plain; charset=utf-8"},
		{"/ld/own", "", http.StatusOK, "application/json"},
		{"/plain", "application/ld+json", http.StatusNotAcceptable, "text/plain; charset=utf-8"},
	} {
		var header http.Header
		if c.accept != "" {
			header = http.Header{"Accept": {c.accept}}
		}
		w := send(t, r, call{method: "GET", target: c.target, header: header})
		if w.Code != c.status || w.Header().Get("Content-Type") != c.contentType {
			t.Errorf("GET %s with Accept %q answered %d %s, want %d %s", c.target, c.accept, w.Code, w.Header().Get("Content-Type"), c.status, c.contentType)
		}
	}
}

func TestCodecsUnusableByInheritedProducesIsASetupError(t *testing.T) {
	r := newRouter(t, h.Config{})
	api := r.Group("/api", h.Produces("application/json"))
	api.Get("/doc", respond(h.OK(item{"a"})), h.Codecs(ldOnly(t)))
	wantSetupError(t, r, `GET /api/doc`)
}

func TestNilCodecsIsASetupError(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", respond(h.OK(item{"a"})), h.Codecs(nil))
	wantSetupError(t, r, "Codecs with a nil set")
}
