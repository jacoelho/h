package h_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/mw"
	"github.com/jacoelho/h/problem"
)

// The code the README shows lives in regions here, marked README begin and
// README end, and TestREADMEGoBlocksMatchTheirRegions keeps the README in
// step with it.

var svc = &OrderService{}

type Query struct {
	Term  string
	Limit int64
	After time.Time
}

func (s *OrderService) Search(ctx context.Context, q Query) ([]Order, error) {
	return s.List(ctx, nil, q.Limit)
}

func (s *OrderService) All(ctx context.Context) iter.Seq2[Order, error] {
	return func(yield func(Order, error) bool) {
		orders, err := s.List(ctx, nil, 0)
		if err != nil {
			yield(Order{}, err)
			return
		}
		for _, o := range orders {
			if !yield(o, nil) {
				return
			}
		}
	}
}

// README begin optional
func optional[T any](parse func(string) (T, error)) func(string) (*T, error) {
	return func(s string) (*T, error) {
		if s == "" {
			return nil, nil
		}
		v, err := parse(s)
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
}

// README end

// README begin refusal-problems
var RefusalProblems = problem.Map(
	problem.As(func(e *codec.DecodeError) problem.Problem {
		p := problem.Problem{Status: http.StatusBadRequest, Title: "Malformed body"}
		if se, ok := errors.AsType[*json.SemanticError](e); ok && se.JSONPointer != "" {
			p.Extra = map[string]any{"pointer": string(se.JSONPointer)}
		}
		return p
	}),
	problem.As(func(e *h.RequestError) problem.Problem {
		p := problem.Problem{Status: e.Status}
		switch {
		case errors.Is(e, h.ErrNoRoute):
			p.Extra = map[string]any{"code": "no_route"}
		case e.Status == http.StatusRequestEntityTooLarge:
			p.Extra = map[string]any{"code": "body_too_large"}
		}
		return p
	}),
)

// README end

// README begin create-order-test
func TestCreateOrder(t *testing.T) {
	var errs htest.Errors
	r, err := h.New(h.Config{BasePath: "/shop", OnError: errs.OnError})
	if err != nil {
		t.Fatal(err)
	}
	RegisterOrders(r, svc)
	c := htest.New(t, r)

	o := c.Do("POST", "/orders", NewOrder{Item: "tea"}).Expect(t, http.StatusCreated).JSON[Order](t)
	if o.Item != "tea" {
		t.Fatalf("created %+v", o)
	}

	p := c.Do("GET", "/orders?limit=x", nil).Expect(t, http.StatusBadRequest).Problem(t)
	if p.Title != "Bad Request" {
		t.Fatalf("problem %+v", p)
	}
	if len(errs.All()) != 1 {
		t.Fatalf("OnError calls: %+v", errs.All())
	}
}

// README end

func fetch(handler http.Handler, target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func mustHandler(r *h.Router) http.Handler {
	handler, err := r.Handler()
	if err != nil {
		log.Fatal(err)
	}
	return handler
}

func ExampleRouter_Group() {
	r, _ := h.New(h.Config{BasePath: "/shop"})
	// README begin group
	orders := r.Group("/orders", h.Errors(OrderErrors))

	orders.Post("/", func(ctx context.Context, req h.Request[NewOrder]) (h.Response[Order], error) {
		o, err := svc.Create(ctx, req.Body())
		if err != nil {
			return h.Response[Order]{}, err
		}
		return h.Created(req.Link("orders", string(o.ID)), o), nil
	})
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleRequest_Parse() {
	r, _ := h.New(h.Config{})
	orders := r.Group("/orders")
	// README begin parse
	orders.Get("/", func(ctx context.Context, req h.Request[h.Empty]) (h.Response[[]Order], error) {
		var q Query
		if err := req.Parse(func(p *h.Params) {
			q.Term = p.Query("q", h.String)                        // required
			q.Limit = p.QueryOr("limit", 20, h.Int64)              // default when absent
			q.After, _ = p.QueryOpt("after", h.Time(time.RFC3339)) // optional
		}); err != nil {
			return h.Response[[]Order]{}, err
		}
		found, err := svc.Search(ctx, q)
		return h.OK(found), err
	})
	// README end
	p, err := problem.Read(fetch(mustHandler(r), "/orders?limit=x").Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(p.Status)
	params, ok := p.Extra["invalid_params"].([]any)
	if !ok {
		log.Fatalf("invalid_params is %T, want a list", p.Extra["invalid_params"])
	}
	for _, v := range params {
		param, ok := v.(map[string]any)
		if !ok {
			log.Fatalf("invalid_params element is %T, want an object", v)
		}
		fmt.Println(param["in"], param["name"], param["reason"])
	}
	// Output:
	// 400
	// query q required
	// query limit invalid
}

func ExampleParams_QueryOr() {
	r, _ := h.New(h.Config{})
	r.Get("/orders", h.Bind(func(p *h.Params, _ h.Empty) *time.Time {
		// README begin optional-use
		after := p.QueryOr("after", nil, optional(h.Time(time.RFC3339))) // nil when absent or empty
		// README end
		return after
	}, func(_ context.Context, after *time.Time) (string, error) {
		if after == nil {
			return "from the first order", nil
		}
		return "after " + after.Format(time.DateOnly), nil
	}))
	handler := mustHandler(r)
	for _, target := range []string{"/orders", "/orders?after=", "/orders?after=2026-11-23T10:00:00Z"} {
		fmt.Println(fetch(handler, target, "Accept", "text/plain").Body.String())
	}
	// Output:
	// from the first order
	// from the first order
	// after 2026-11-23
}

func Example_problemError() {
	r, _ := h.New(h.Config{})
	stock := 0
	r.Post("/orders", func(ctx context.Context, req h.Request[NewOrder]) (h.Response[Order], error) {
		// README begin out-of-stock
		if stock == 0 {
			return h.Response[Order]{}, problem.Error(problem.Problem{Status: http.StatusConflict, Title: "Out of stock"})
		}
		// README end
		return h.OK(Order{Item: req.Body().Item}), nil
	})
	_ = mustHandler(r)
	// Output:
}

func ExampleConfig_requestErrors() {
	// README begin refusal-config
	r, err := h.New(h.Config{RequestErrors: RefusalProblems})
	// README end
	if err != nil {
		log.Fatal(err)
	}
	r.Fallback()
	fmt.Println(fetch(mustHandler(r), "/missing").Body.String())
	// Output: {"type":"about:blank","title":"Not Found","status":404,"code":"no_route"}
}

func ExampleRawBody() {
	r, _ := h.New(h.Config{})
	// README begin photos
	r.Post("/photos", func(ctx context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		f, err := os.CreateTemp("", "photo-*")
		if err != nil {
			return h.Response[h.Empty]{}, err
		}
		_, err = io.Copy(f, req.Body().Reader())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return h.NoContent[h.Empty](), err
	}, h.Consumes("image/*"), h.MaxBody(10<<20))
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleItems() {
	r, _ := h.New(h.Config{})
	orders := r.Group("/orders")
	// README begin export
	orders.Get("/export", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[Order]], error) {
		return h.OK(h.Items(svc.All(ctx))), nil
	})
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleCodecs() {
	r, _ := h.New(h.Config{})
	// README begin jsonld
	ld := codec.NewSet()
	if err := ld.Encodes(codec.JSON{Options: []json.Options{json.Deterministic(true)}}, "application/ld+json"); err != nil {
		log.Fatal(err)
	}

	r.Get("/orders.jsonld", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[Order]], error) {
		return h.OK(h.Items(svc.All(ctx))).
			WithHeader("Content-Disposition", `attachment; filename="orders.jsonld"`), nil
	}, h.Codecs(ld))
	// README end
	r.Get("/orders", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[[]Order], error) {
		return h.OK([]Order{}), nil
	})
	handler := mustHandler(r)
	fmt.Println(fetch(handler, "/orders.jsonld").Header().Get("Content-Type"))
	fmt.Println(fetch(handler, "/orders", "Accept", "application/ld+json").Code)
	// Output:
	// application/ld+json
	// 406
}

func ExampleNewStream() {
	r, _ := h.New(h.Config{})
	// README begin csv
	r.Get("/report.csv", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/csv", func(w *h.StreamWriter) error {
			_, err := io.WriteString(w, "id,item\n")
			return err
		})).WithHeader("Content-Disposition", `attachment; filename="report.csv"`), nil
	})
	// README end
	w := fetch(mustHandler(r), "/report.csv")
	fmt.Print(w.Header().Get("Content-Type"), "\n", w.Body.String())
	// Output:
	// text/csv
	// id,item
}

func ExampleRouter_Handle() {
	r, _ := h.New(h.Config{})
	// README begin handle
	r.Handle("GET", "/static/{file...}", func(w http.ResponseWriter, req *http.Request) error {
		http.ServeFileFS(w, req, os.DirFS("static"), req.PathValue("file"))
		return nil
	})
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleRaw() {
	r, _ := h.New(h.Config{})
	davHandler := http.NotFoundHandler()
	// README begin raw
	r.Handle("GET", "/healthz", h.Raw(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r.Handle("PROPFIND", "/dav/{path...}", h.Raw(davHandler.ServeHTTP))
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleCORS() {
	// README begin cors
	r, err := h.New(h.Config{
		TrustedOrigins: []string{"https://app.example.com"},
		CORS:           h.CORS{AllowCredentials: true},
	})
	// README end
	if err != nil {
		log.Fatal(err)
	}
	_ = r
	// Output:
}

func ExampleRouter_Use() {
	r, _ := h.New(h.Config{})
	logger := slog.New(slog.DiscardHandler)
	// README begin middleware
	r.Use(mw.RequestID("X-Request-Id"), mw.AccessLog(logger, slog.LevelInfo), mw.Timeout(10*time.Second))
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleConfig_annotate() {
	cfg := h.Config{
		// README begin annotate
		Annotate: func(f h.Failure) problem.Problem {
			if id, ok := mw.RequestIDFrom(f.Request.Context()); ok {
				return f.Problem.With("request_id", id)
			}
			return f.Problem
		},
		// README end
	}
	r, _ := h.New(cfg)
	r.Use(mw.RequestID("X-Request-Id"))
	r.Fallback()
	fmt.Println(fetch(mustHandler(r), "/missing", "X-Request-Id", "req-1").Body.String())
	// Output: {"type":"about:blank","title":"Not Found","status":404,"request_id":"req-1"}
}

func ExampleFailure_Hidden() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	cfg := h.Config{
		SpillAt: 16,
		// README begin onerror
		OnError: func(f h.Failure) {
			if f.Problem.Status < http.StatusInternalServerError && !f.Hidden() {
				return
			}
			logger.ErrorContext(f.Request.Context(), "request failed", "status", f.Problem.Status, "err", f.Err)
		},
		// README end
	}
	r, _ := h.New(cfg)
	r.Get("/orders/{id}", h.Bind(func(p *h.Params, _ h.Empty) OrderID { return p.Path("id", ParseOrderID) }, svc.Get))
	r.Get("/report.csv", func(context.Context, h.Request[h.Empty]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/csv", func(w *h.StreamWriter) error {
			if _, err := io.WriteString(w, "id,item\no-1,tea\no-2,coffee\n"); err != nil {
				return err
			}
			return errors.New("reading orders: query failed")
		})), nil
	})
	handler := mustHandler(r)
	fetch(handler, "/orders/not-an-id")
	func() {
		defer func() { _ = recover() }()
		fetch(handler, "/report.csv")
	}()
	// Output: level=ERROR msg="request failed" status=0 err="h: response failed after it started: reading orders: query failed"
}

func ExampleRouter_Mount() {
	r, _ := h.New(h.Config{BasePath: "/shop"})
	handler := mustHandler(r)
	// README begin mount
	mux := http.NewServeMux()
	mux.Handle(r.Mount(), handler) // "/shop/" for BasePath "/shop"
	// README end
	_ = mux
	// Output:
}

func ExampleRouter_Get_singlePageApp() {
	r, _ := h.New(h.Config{BasePath: "/shop"})
	listOrders := h.Func(func(context.Context, h.Empty) ([]Order, error) { return []Order{}, nil })
	app := http.NotFoundHandler()
	// README begin single-page-app
	r.Get("/api/orders", listOrders)
	r.Handle("GET", "/{path...}", h.Raw(app.ServeHTTP)) // every other GET, such as /shop/recipes/42
	// README end
	_ = mustHandler(r)
	// Output:
}

func ExampleRouter_Handler() {
	r, _ := h.New(h.Config{})
	// README begin handler
	handler, err := r.Handler()
	if err != nil {
		log.Fatal(err) // lists every mistake with its route
	}
	// README end
	_ = handler
	// Output:
}
