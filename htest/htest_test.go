package htest_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
)

type note struct {
	Text string `json:"text"`
}

type seen struct {
	path, contentType, trace string
}

func TestDoJoinsBasePathAndSendsJSON(t *testing.T) {
	r, err := h.New(h.Config{BasePath: "/api"})
	if err != nil {
		t.Fatal(err)
	}
	var got seen
	r.Post("/notes", func(_ context.Context, req h.Request[note]) (h.Response[note], error) {
		hr := req.HTTP()
		got = seen{hr.URL.Path, hr.Header.Get("Content-Type"), hr.Header.Get("X-Trace")}
		return h.Created(req.Link("notes", "1"), req.Body()), nil
	})
	resp := htest.New(t, r).Do("POST", "/notes", note{Text: "hi"}, htest.Header("X-Trace", "t1")).Expect(t, http.StatusCreated)
	if want := (seen{"/api/notes", "application/json", "t1"}); got != want {
		t.Fatalf("router saw %+v, want %+v", got, want)
	}
	if n := resp.JSON[note](t); n.Text != "hi" {
		t.Fatalf("body %+v", n)
	}
}

// recordingTB stands in for a test whose failures are under test.
type recordingTB struct {
	testing.TB
	failed string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	panic(r)
}

func (r *recordingTB) Fatal(args ...any) { r.Fatalf("%s", fmt.Sprint(args...)) }

func failure(f func(tb testing.TB)) (msg string) {
	tb := &recordingTB{}
	defer func() {
		if v := recover(); v != nil && v != tb {
			panic(v)
		}
		msg = tb.failed
	}()
	f(tb)
	return ""
}

func TestResponseChecksFail(t *testing.T) {
	r, err := h.New(h.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.Get("/text", func(context.Context, h.Request[h.Empty]) (h.Response[string], error) { return h.OK("plain"), nil })
	r.Get("/missing", func(context.Context, h.Request[h.Empty]) (h.Response[note], error) {
		return h.Response[note]{}, &h.RequestError{Status: http.StatusNotFound}
	})
	c := htest.New(t, r)
	text := c.Do("GET", "/text", nil, htest.Header("Accept", "text/plain"))

	if msg := failure(func(tb testing.TB) {
		tb.Helper()
		text.Expect(tb, http.StatusCreated)
	}); msg == "" {
		t.Fatal("Expect passed on a wrong status")
	}
	if msg := failure(func(tb testing.TB) {
		tb.Helper()
		text.Problem(tb)
	}); msg == "" {
		t.Fatal("Problem passed on a text body")
	}
	if msg := failure(func(tb testing.TB) {
		tb.Helper()
		text.JSON[note](tb)
	}); msg == "" {
		t.Fatal("JSON passed on a text body")
	}
	if p := c.Do("GET", "/missing", nil).Expect(t, http.StatusNotFound).Problem(t); p.Status != http.StatusNotFound || p.Title != "Not Found" {
		t.Fatalf("problem %+v", p)
	}
}

func TestNewFailsOnSetupErrors(t *testing.T) {
	r, err := h.New(h.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.Get("no-slash", func(context.Context, h.Request[h.Empty]) (h.Response[note], error) { return h.Response[note]{}, nil })
	if msg := failure(func(tb testing.TB) {
		tb.Helper()
		htest.New(tb, r)
	}); msg == "" {
		t.Fatal("New passed a router with setup errors")
	}
}

func TestErrorsRecordsOnError(t *testing.T) {
	var errs htest.Errors
	r, err := h.New(h.Config{OnError: errs.OnError})
	if err != nil {
		t.Fatal(err)
	}
	r.Get("/x", func(context.Context, h.Request[h.Empty]) (h.Response[note], error) {
		return h.Response[note]{}, fmt.Errorf("boom")
	})
	htest.New(t, r).Do("GET", "/x", nil).Expect(t, http.StatusInternalServerError)
	if all := errs.All(); len(all) != 1 || all[0].Problem.Status != 500 || errs.Last(t).Err.Error() != "boom" {
		t.Fatalf("recorded %+v", all)
	}
	if msg := failure(func(tb testing.TB) {
		tb.Helper()
		(&htest.Errors{}).Last(tb)
	}); msg == "" {
		t.Fatal("Last passed with nothing recorded")
	}
}
