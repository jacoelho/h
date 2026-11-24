package h_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

var errSecret = errors.New("db password is hunter2")

func failing(err error) h.Endpoint[h.Empty, item] {
	return func(context.Context, h.Request[h.Empty]) (h.Response[item], error) {
		return h.Response[item]{}, err
	}
}

func TestProblemFormatFollowsAccept(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		return h.Response[h.Empty]{}, errSecret
	})
	cases := []struct {
		accept, contentType, body string
	}{
		{"", "application/problem+json", `{"type":"about:blank","title":"Internal Server Error","status":500}`},
		{"application/json", "application/problem+json", `{"type":"about:blank","title":"Internal Server Error","status":500}`},
		{"text/plain", "text/plain; charset=utf-8", "Internal Server Error\n"},
		{"image/png", "text/plain; charset=utf-8", "Internal Server Error\n"},
	}
	for _, c := range cases {
		t.Run(c.accept, func(t *testing.T) {
			hdr := http.Header{}
			if c.accept != "" {
				hdr.Set("Accept", c.accept)
			}
			w := send(t, r, call{method: "GET", target: "/x", header: hdr})
			w.Expect(t, http.StatusInternalServerError)
			if ct := w.Header().Get("Content-Type"); ct != c.contentType {
				t.Fatalf("Content-Type %q, want %q", ct, c.contentType)
			}
			if strings.HasPrefix(c.contentType, "application") {
				wantJSON(t, w.Body.Bytes(), c.body)
			} else if w.Body.String() != c.body {
				t.Fatalf("body %q, want %q", w.Body, c.body)
			}
			if strings.Contains(w.Body.String(), "hunter2") {
				t.Fatal("internal error text leaked")
			}
			if w.Header().Get("Vary") != "Accept" {
				t.Fatalf("Vary %q", w.Header().Get("Vary"))
			}
		})
	}
}

func TestGroupProblemFormat(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	legacy := problem.JSON("application/vnd.legacy+json", func(p problem.Problem) any {
		return struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{p.Status, p.TitleOrStatus()}
	})
	notFound := h.Errors(problem.Is(ErrOrderNotFound, problem.Problem{Status: 404}))
	r.Group("/legacy", notFound, h.ProblemFormats(legacy)).Get("/x", failing(ErrOrderNotFound))
	r.Group("/modern", notFound).Get("/x", failing(ErrOrderNotFound))

	w := send(t, r, call{method: "GET", target: "/legacy/x", header: http.Header{"Accept": {"application/json"}}})
	w.Expect(t, http.StatusNotFound)
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.legacy+json" {
		t.Fatalf("Content-Type %q", ct)
	}
	wantJSON(t, w.Body.Bytes(), `{"code":404,"message":"Not Found"}`)
	legacySeen := log.Last(t).Problem

	w = send(t, r, call{method: "GET", target: "/modern/x", header: http.Header{"Accept": {"application/json"}}})
	w.Expect(t, http.StatusNotFound)
	w.Problem(t)
	if modern := log.Last(t).Problem; modern.Status != legacySeen.Status || modern.Type != legacySeen.Type {
		t.Fatalf("OnError saw %+v and %+v for the same error", legacySeen, modern)
	}
}

type brokenFormat struct{}

func (brokenFormat) MediaType() string                      { return "application/problem+json" }
func (brokenFormat) Write(io.Writer, problem.Problem) error { return errors.New("broken") }

func TestFailingProblemFormatFallsBackToText(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, ProblemFormats: []problem.Format{brokenFormat{}}})
	r.Get("/x", failing(errSecret))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusInternalServerError)
	if ct, body := w.Header().Get("Content-Type"), w.Body.String(); ct != "text/plain; charset=utf-8" || body != "Internal Server Error\n" {
		t.Fatalf("fallback %q %q", ct, body)
	}
	if !errors.Is(log.Last(t).Err, h.ErrProblemFormat) {
		t.Fatalf("OnError got %v, want ErrProblemFormat", log.Last(t).Err)
	}
}

func TestMapperPrecedence(t *testing.T) {
	r := newRouter(t, h.Config{Errors: problem.Is(ErrOutOfStock, problem.Problem{Status: 500, Title: "router"})})
	g := r.Group("/g", h.Errors(problem.Is(ErrOutOfStock, problem.Problem{Status: 503, Title: "group"})))
	g.Get("/route", failing(ErrOutOfStock), h.Errors(problem.Is(ErrOutOfStock, problem.Problem{Status: 409, Title: "route"})))
	g.Get("/group", failing(ErrOutOfStock))
	r.Get("/root", failing(ErrOutOfStock))
	for target, title := range map[string]string{"/g/route": "route", "/g/group": "group", "/root": "router"} {
		w := send(t, r, call{method: "GET", target: target})
		if got := w.Problem(t).Title; got != title {
			t.Fatalf("%s answered by %v, want %s", target, got, title)
		}
	}
}

func TestRequestErrorsMappersDescribeRefusalsWithA4xxStatus(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{
		OnError: log.OnError,
		MaxBody: 8,
		Errors: problem.Func(func(error) (problem.Problem, bool) {
			return problem.Problem{Status: http.StatusInternalServerError, Title: "catch-all"}, true
		}),
		RequestErrors: problem.Map(
			problem.As(func(*h.ParamsError) problem.Problem { return problem.Problem{Status: 422} }),
			problem.As(func(e *h.RequestError) problem.Problem {
				switch e.Status {
				case http.StatusRequestEntityTooLarge:
					return problem.Problem{Status: 413, Detail: "orders are at most 8 bytes"}
				case http.StatusUnsupportedMediaType:
					return problem.Problem{Status: http.StatusInternalServerError}
				}
				return problem.Problem{Status: 400, Title: "rewritten"}
			}),
		),
	})
	r.Get("/q", func(_ context.Context, req h.Request[h.Empty]) (h.Response[item], error) {
		return h.Response[item]{}, req.Parse(func(p *h.Params) { p.Query("n", h.Int64) })
	})
	r.Post("/items", echo(nil))

	send(t, r, call{method: "GET", target: "/q"}).Expect(t, http.StatusUnprocessableEntity)

	w := send(t, r, call{method: "POST", target: "/items", body: `{"name":"long"}`, header: jsonHeader()})
	w.Expect(t, http.StatusRequestEntityTooLarge)
	if d := w.Problem(t).Detail; d != "orders are at most 8 bytes" {
		t.Fatalf("detail %v", d)
	}

	w = send(t, r, call{method: "POST", target: "/items", body: `{`, header: jsonHeader()})
	w.Expect(t, http.StatusBadRequest)
	if title := w.Problem(t).Title; title != "rewritten" {
		t.Fatalf("title %v, want the RequestErrors mapper's", title)
	}

	w = send(t, r, call{method: "POST", target: "/items", body: `x`, header: http.Header{"Content-Type": {"text/xml"}}})
	w.Expect(t, http.StatusUnsupportedMediaType)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v, want ErrInvalidProblem for a 5xx answer", log.Last(t).Err)
	}
}

func TestUnmappedErrorIsInternal(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	wrapped := fmt.Errorf("loading: %w", errSecret)
	r.Get("/x", failing(wrapped))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusInternalServerError)
	seen := log.Last(t)
	if !errors.Is(seen.Err, errSecret) || seen.Problem.Status != 500 {
		t.Fatalf("OnError got %v, %+v", seen.Err, seen.Problem)
	}
}

func TestInvalidMapperResultIsInternal(t *testing.T) {
	cases := map[string]problem.Problem{
		"success status": {Status: 200},
		"relative type":  {Status: 400, Type: "/probs/x"},
		"reserved extra": {Status: 400, Extra: map[string]any{"status": 1}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			var log htest.Errors
			r := newRouter(t, h.Config{OnError: log.OnError, Errors: problem.Func(func(error) (problem.Problem, bool) { return p, true })})
			r.Get("/x", failing(errSecret))
			w := send(t, r, call{method: "GET", target: "/x"})
			w.Expect(t, http.StatusInternalServerError)
			if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
				t.Fatalf("OnError got %v", log.Last(t).Err)
			}
		})
	}
}

func TestProblemHeaderIsSent(t *testing.T) {
	errAuth := errors.New("no credentials")
	r := newRouter(t, h.Config{Errors: problem.Is(errAuth, problem.Problem{Status: 401, Header: http.Header{"Www-Authenticate": {`Bearer realm="api"`}}})})
	r.Get("/x", failing(errAuth))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusUnauthorized)
	if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="api"` {
		t.Fatalf("WWW-Authenticate %q", got)
	}
}

func TestContextEndings(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Get("/wait", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[item], error) {
		<-ctx.Done()
		return h.Response[item]{}, ctx.Err()
	})
	r.Get("/downstream", failing(fmt.Errorf("calling billing: %w", context.DeadlineExceeded)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := htest.New(t, r)
	w := client.Send(httptest.NewRequestWithContext(ctx, "GET", "/wait", nil))
	w.Expect(t, 499)
	if w.Body.Len() != 0 {
		t.Fatalf("499 with body %q", w.Body)
	}
	if !errors.Is(log.Last(t).Err, context.Canceled) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 0)
	defer cancel()
	client.Send(httptest.NewRequestWithContext(ctx, "GET", "/wait", nil)).Expect(t, http.StatusServiceUnavailable)

	w = send(t, r, call{method: "GET", target: "/downstream"})
	w.Expect(t, http.StatusInternalServerError)
}

func TestEndpointErrorsKeepTheirProvenance(t *testing.T) {
	cases := map[string]error{
		"own read timeout":   fmt.Errorf("reading upstream: %w", os.ErrDeadlineExceeded),
		"own decode error":   &codec.DecodeError{Err: errors.New("bad upstream payload")},
		"own size limit":     &http.MaxBytesError{Limit: 10},
		"constructed in Get": fmt.Errorf("wrapped: %w", errSecret),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRouter(t, h.Config{})
			r.Get("/x", failing(err))
			send(t, r, call{method: "GET", target: "/x"}).Expect(t, http.StatusInternalServerError)
		})
	}
}

func TestEndpointMayRefuseWithRequestError(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/x", failing(&h.RequestError{Status: http.StatusRequestEntityTooLarge, Err: errors.New("too many items")}))
	send(t, r, call{method: "GET", target: "/x"}).Expect(t, http.StatusRequestEntityTooLarge)
}

func TestPanicBecomesProblem(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	var status int
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, req)
			status = rec.Code
			maps.Copy(w.Header(), rec.Header())
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
		})
	})
	r.Get("/boom", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) { panic("boom") })
	r.Get("/abort", func(context.Context, h.Request[h.Empty]) (h.Response[item], error) { panic(http.ErrAbortHandler) })

	w := send(t, r, call{method: "GET", target: "/boom"})
	w.Expect(t, http.StatusInternalServerError)
	if status != 500 {
		t.Fatalf("middleware saw %d", status)
	}
	perr, ok := errors.AsType[*h.PanicError](log.Last(t).Err)
	if !ok || !errors.Is(perr, h.ErrPanic) || perr.Value != "boom" || len(perr.Stack) == 0 || !strings.Contains(perr.Error(), "boom") {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}

	if !send(t, r, call{method: "GET", target: "/abort"}).Aborted {
		t.Fatal("ErrAbortHandler from an endpoint did not abort the response")
	}
}

type panickingFormat struct{}

func (panickingFormat) MediaType() string                      { return "application/problem+json" }
func (panickingFormat) Write(io.Writer, problem.Problem) error { panic("format broke") }

func TestPanickingMapperAndFormatAreContained(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{
		OnError:        log.OnError,
		ProblemFormats: []problem.Format{panickingFormat{}},
		Errors:         problem.Func(func(error) (problem.Problem, bool) { panic("mapper broke") }),
	})
	r.Get("/x", failing(errSecret))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, http.StatusInternalServerError)
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q, want the text fallback", ct)
	}
	if !errors.Is(log.All()[0].Err, h.ErrPanic) || !errors.Is(log.All()[0].Err, errSecret) {
		t.Fatalf("OnError got %v, want the mapper panic and the original error", log.All()[0].Err)
	}
	if !errors.Is(log.Last(t).Err, h.ErrProblemFormat) {
		t.Fatalf("OnError got %v, want ErrProblemFormat", log.Last(t).Err)
	}
}

func TestInvalidRouterMapperFailsNew(t *testing.T) {
	_, err := h.New(h.Config{Errors: problem.Is(errSecret, problem.Problem{Status: 200})})
	if !errors.Is(err, problem.ErrInvalid) {
		t.Fatalf("New error %v, want problem.ErrInvalid", err)
	}
}

func TestComputedProblemIsCheckedAtRequestTime(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError, Errors: problem.As(func(*limitedError) problem.Problem { return problem.Problem{Status: 200} })})
	r.Get("/x", failing(&limitedError{}))
	send(t, r, call{method: "GET", target: "/x"}).Expect(t, http.StatusInternalServerError)
	if !errors.Is(log.Last(t).Err, h.ErrInvalidProblem) {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}
}

type limitedError struct{}

func (*limitedError) Error() string { return "limited" }

func TestAnUnmappedErrorAfterCancellationIsAnsweredWithABare499(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Get("/wait", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[item], error) {
		<-ctx.Done()
		return h.Response[item]{}, errors.New("driver: bad connection")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := htest.New(t, r).Send(httptest.NewRequestWithContext(ctx, "GET", "/wait", nil))
	w.Expect(t, 499)
	if w.Body.Len() != 0 {
		t.Fatalf("499 with body %q", w.Body)
	}
}

func TestA499AnErrorsMapperChoosesIsWrittenInFull(t *testing.T) {
	errGone := errors.New("gone")
	r := newRouter(t, h.Config{Errors: problem.Is(errGone, problem.Problem{Status: 499, Title: "Gone fishing"})})
	r.Get("/x", failing(errGone))
	w := send(t, r, call{method: "GET", target: "/x"})
	w.Expect(t, 499)
	if !strings.Contains(w.Body.String(), "Gone fishing") {
		t.Fatalf("body %q", w.Body)
	}
}
