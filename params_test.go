package h_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/jacoelho/h"
)

func TestParamsReportEveryFailureInOrder(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/items", func(_ context.Context, req h.Request[h.Empty]) (h.Response[item], error) {
		err := req.Parse(func(p *h.Params) {
			p.Query("limit", h.Int64)
			p.Query("page", h.Int64)
			p.Header("X-Tenant", h.String)
			p.QueryOr("sort", "name", h.OneOf("name", "date"))
		})
		return h.Response[item]{}, err
	})
	w := send(t, r, call{method: "GET", target: "/items?limit=ten&sort=size"})
	w.Expect(t, http.StatusBadRequest)
	wantJSON(t, w.Body.Bytes(), `{"type":"about:blank","title":"Bad Request","status":400,"invalid_params":[
		{"in":"query","name":"limit","reason":"invalid"},
		{"in":"query","name":"page","reason":"required"},
		{"in":"header","name":"X-Tenant","reason":"required"},
		{"in":"query","name":"sort","reason":"invalid"}]}`)
}

func TestQueryValues(t *testing.T) {
	type got struct {
		Limit   string
		First   string
		All     []string
		Opt     bool
		Present string
		Found   bool
		Default int64
		Header  int64
	}
	var g got
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x?limit=&tag=a&tag=b", http.NoBody)
	req.Header.Add("X-N", "7")
	req.Header.Add("X-N", "8")
	err := h.NewRequest(req, h.Empty{}).Parse(func(p *h.Params) {
		g.Limit = p.Query("limit", h.String)
		g.First = p.Query("tag", h.String)
		g.All = p.QueryAll("tag", h.String)
		_, g.Opt = p.QueryOpt("missing", h.String)
		g.Present, g.Found = p.QueryOpt("tag", h.String)
		g.Default = p.QueryOr("page", 3, h.Int64)
		g.Header = p.Header("X-N", h.Int64)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := got{Limit: "", First: "a", All: []string{"a", "b"}, Opt: false, Present: "a", Found: true, Default: 3, Header: 7}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("got %+v, want %+v", g, want)
	}
}

func TestEmptyValueReachesParser(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x?limit=", http.NoBody)
	err := h.NewRequest(req, h.Empty{}).Parse(func(p *h.Params) { p.Query("limit", h.Int64) })
	pe, ok := errors.AsType[*h.ParamsError](err)
	if !ok || len(pe.Params) != 1 || errors.Is(pe.Params[0].Err, h.ErrMissing) {
		t.Fatalf("got %v, want one parse failure (not missing)", err)
	}
	if _, ok := errors.AsType[*strconv.NumError](pe.Params[0].Err); !ok {
		t.Fatalf("parser error not kept: %v", pe.Params[0].Err)
	}
}

func TestMalformedQuery(t *testing.T) {
	var called bool
	r := newRouter(t, h.Config{})
	r.Get("/bind", h.Bind(func(p *h.Params, _ h.Empty) int64 { return p.QueryOr("n", 1, h.Int64) },
		func(context.Context, int64) (item, error) { called = true; return item{}, nil }))
	r.Get("/ignores", respond(h.OK(item{"ok"})))
	send(t, r, call{method: "GET", target: "/bind?n=%zz"}).Expect(t, http.StatusBadRequest)
	if called {
		t.Fatal("domain function called after a Parse failure")
	}
	send(t, r, call{method: "GET", target: "/ignores?n=%zz"}).Expect(t, http.StatusOK)
}

func TestInvalidPathValueIsNotFound(t *testing.T) {
	var called bool
	r := newRouter(t, h.Config{})
	r.Get("/orders/{id}", h.Bind(func(p *h.Params, _ h.Empty) OrderID {
		p.Query("expand", h.String)
		return p.Path("id", ParseOrderID)
	}, func(context.Context, OrderID) (Order, error) { called = true; return Order{}, nil }))
	w := send(t, r, call{method: "GET", target: "/orders/not-an-id"})
	w.Expect(t, http.StatusNotFound)
	w.Problem(t)
	if called {
		t.Fatal("domain function called")
	}
	send(t, r, call{method: "GET", target: "/orders/o-1"}).Expect(t, http.StatusBadRequest)
}

func TestLinkEscapesSegments(t *testing.T) {
	r := newRouter(t, h.Config{BasePath: "/api"})
	r.Group("/a").Get("/x", func(_ context.Context, req h.Request[h.Empty]) (h.Response[string], error) {
		return h.OK(req.Link("files", "a b/c")), nil
	})
	w := send(t, r, call{method: "GET", target: "/api/a/x", header: http.Header{"Accept": {"text/plain"}}})
	if w.Body.String() != "/api/files/a%20b%2Fc" {
		t.Fatalf("Link %q", w.Body)
	}
}

func TestZeroValuesAreUsable(t *testing.T) {
	var req h.Request[item]
	if req.HTTP() == nil || req.Link("a") != "/a" {
		t.Fatalf("zero Request: %v %q", req.HTTP(), req.Link("a"))
	}
	err := req.Parse(func(p *h.Params) { p.Query("q", h.String) })
	if _, ok := errors.AsType[*h.ParamsError](err); !ok {
		t.Fatalf("zero Request Parse: %v", err)
	}
	var form h.Form
	err = form.Parse(func(p *h.FormParams) { p.Value("name", h.String) })
	if _, ok := errors.AsType[*h.ParamsError](err); !ok {
		t.Fatalf("zero Form Parse: %v", err)
	}
}

func TestLinkWithoutBaseOrSegmentsIsTheRoot(t *testing.T) {
	if got := h.NewRequest(nil, h.Empty{}).Link(); got != "/" {
		t.Fatalf("Link() = %q, want /", got)
	}
}
