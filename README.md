# h

h is a Go HTTP router whose handlers are typed functions:

```go
func(ctx context.Context, req h.Request[In]) (h.Response[Out], error)
```

The router reads and decodes the request body, calls your function, and writes the result in the media type the client accepts. When your function returns an error, the router answers with an RFC 9457 problem. You decide which errors become which problems, one mapping per business capability.

The module requires Go 1.27, because the API uses generic methods. It has no tagged release yet.

## Install

```sh
go get github.com/jacoelho/h
```

## Serve your first route

This program answers `GET /greet/{name}` with JSON, and answers an unknown name with a 404 problem:

```go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/problem"
)

type Greeting struct {
	Message string `json:"message"`
}

var ErrUnknownName = errors.New("unknown name")

func greet(_ context.Context, name string) (Greeting, error) {
	if name == "nobody" {
		return Greeting{}, ErrUnknownName
	}
	return Greeting{Message: "hello, " + name}, nil
}

func main() {
	r, err := h.New(h.Config{
		Errors: problem.Is(ErrUnknownName, problem.Problem{Status: http.StatusNotFound, Title: "Unknown name"}),
	})
	if err != nil {
		log.Fatal(err)
	}
	r.Get("/greet/{name}", h.Bind(func(p *h.Params, _ h.Empty) string {
		return p.Path("name", h.String)
	}, greet))

	handler, err := r.Handler()
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.ListenAndServe("localhost:8080", handler))
}
```

`greet` knows nothing about HTTP. `h.Bind` builds its input from the request, and `problem.Is` maps its error to a status:

```console
$ curl -i localhost:8080/greet/ana
HTTP/1.1 200 OK
Content-Length: 24
Content-Type: application/json

{"message":"hello, ana"}

$ curl -i localhost:8080/greet/nobody
HTTP/1.1 404 Not Found
Content-Type: application/problem+json
Vary: Accept
Content-Length: 59

{"type":"about:blank","title":"Unknown name","status":404}
```

## Write endpoints

An endpoint has the type `h.Endpoint[In, Out]`. `In` is the request body and `Out` the response body. Use `h.Empty` for no body.

There are three ways to write one:

- **A plain function.** Write the full signature yourself when you need the request, such as for `req.Link` or response headers.
- **`h.Func(f)`.** Lifts `func(ctx, In) (Out, error)` when the input is the whole body. It answers 200, or 204 when `Out` is `h.Empty`.
- **`h.Bind(bind, f)`.** Lifts a domain function whose input you build from the body and the path, query and header values.

Register endpoints with `Get`, `Delete`, `Post`, `Put` and `Patch`. `Get` and `Delete` take no body, so their `In` is `h.Empty`. Group routes that share a prefix and options:

```go
orders := r.Group("/orders", h.Errors(OrderErrors))

orders.Post("/", func(ctx context.Context, req h.Request[NewOrder]) (h.Response[Order], error) {
	o, err := svc.Create(ctx, req.Body())
	if err != nil {
		return h.Response[Order]{}, err
	}
	return h.Created(req.Link("orders", o.ID), o), nil
})
```

Responses are values: `h.OK`, `h.Created`, `h.Accepted`, `h.NoContent`, `h.NotModified` and `h.Status`. Add header fields with `WithHeader`. The router owns `Content-Type`, `Content-Length` and the CORS fields, so a response that sets them is answered with 500.

## Read path, query and header values

Read every value inside one `Parse` call. The call reports all missing or malformed values together:

```go
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
```

The router answers that error with a 400 problem that lists each value under `invalid_params`. A path value that fails is answered with 404 instead, since the path then names no resource.

A parser is any `func(string) (T, error)`. The package provides `h.String`, `h.Int64`, `h.Time(layout)` and `h.OneOf(values...)`.

## Translate errors into problems

Give each business capability one `problem.Mapper` and attach it to that capability's group:

```go
var OrderErrors = problem.Map(
	problem.Is(ErrOrderNotFound, problem.Problem{
		Status: http.StatusNotFound,
		Type:   "https://example.com/probs/order-not-found",
		Title:  "Order not found",
	}),
	problem.As(func(e *LimitError) problem.Problem {
		p := problem.Problem{Status: http.StatusTooManyRequests, Header: http.Header{}}
		p.Header.Set("Retry-After", "60")
		return p
	}),
)
```

The mapper constructors are:

- **`problem.Is(target, p)`.** Matches like `errors.Is`.
- **`problem.As(f)`.** Matches a typed error and builds the problem from it.
- **`problem.Func(f)`.** Wraps a hand-written `func(error) (Problem, bool)`.
- **`problem.Map(...)`.** Tries mappers in order.

The route's mappers are tried first, then its groups', then `Config.Errors`.

An error no mapper knows is answered with 500. Its text goes only to `Config.OnError`, never to the client. A panic in an endpoint is answered with 500 and reported as `h.ErrPanic`. When the client has gone, the router answers 499. When the request's deadline passed, it answers 503.

A mapper with a fixed problem is checked when it is built. Assert that in the capability's own tests:

```go
func TestOrderErrorsAreValid(t *testing.T) {
	if err := OrderErrors.Err(); err != nil {
		t.Fatal(err)
	}
}
```

A problem that `problem.As` or `problem.Func` computes is checked only at request time. If it is invalid, the router answers 500 and reports `h.ErrInvalidProblem`.

Problems are written as `application/problem+json` by default. To keep an error shape your clients already read, set `Config.ProblemFormats`, or use the `h.ProblemFormats` option on a group, with `problem.JSON(mediaType, shape)`. `problem.Text` is always the last resort. `problem.Read` decodes a problem body, for clients and tests.

## Read forms, uploads and sequences

The type of `In` selects how the body is read:

| `In` | Body | Notes |
|---|---|---|
| a struct or other value | decoded by a codec chosen from `Content-Type` | JSON and `text/plain` by default |
| `h.Form` | `application/x-www-form-urlencoded` or `multipart/form-data` | read values with `req.Body().Parse`; temporary files are removed after the request |
| `h.RawBody` | the bytes as sent | the route must set `h.Consumes` |
| `h.Seq[T]` | a JSON array or NDJSON, read one element at a time | range over `req.Body().All()` |

```go
r.Post("/photos", func(ctx context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
	f, err := os.CreateTemp("", "photo-*")
	if err != nil {
		return h.Response[h.Empty]{}, err
	}
	defer f.Close()
	_, err = io.Copy(f, req.Body().Reader())
	return h.NoContent[h.Empty](), err
}, h.Consumes("image/*"), h.MaxBody(10<<20))
```

Reading the body is bounded by `MaxBody`. A body that is malformed, too large or too slow is answered with 400, 413 or 408. For `h.RawBody` and `h.Seq[T]`, which the endpoint reads itself, that happens when the endpoint returns the read error.

## Stream a response

Return `h.Seq[T]` to write a JSON array or NDJSON element by element. The router ranges over the source after the endpoint returns, so the source must honour its context:

```go
orders.Get("/export", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[Order]], error) {
	return h.OK(h.Items(svc.All(ctx))), nil
})
```

NDJSON is not in the default codec set. Register it with `codec.Set.Encodes` and `Decodes`, and pass the set as `Config.Codecs`.

Return `h.Stream` to write bytes in a content type of your own:

```go
r.Get("/report.csv", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Stream], error) {
	return h.OK(h.NewStream("text/csv", func(w *h.StreamWriter) error {
		_, err := io.WriteString(w, "id,item\n")
		return err
	})).WithHeader("Content-Disposition", `attachment; filename="report.csv"`), nil
})
```

Responses up to `SpillAt` bytes are buffered and sent with `Content-Length`. While the response is still buffered, an encoding error becomes a problem. Once the response has started, an error aborts the connection, so the client sees a failed response rather than a truncated one that looks complete.

For a handler that must write the response itself, such as `http.FileServer`, use `Handle`:

```go
r.Handle("GET", "/static/{file...}", h.Raw(http.FileServer(http.Dir("static"))))
```

## Set limits and media types

Options apply to a group or a single route. The closest setting wins:

| Option | Default | Effect |
|---|---|---|
| `h.MaxBody(n)` | `Config.MaxBody`, 1 MiB | request bodies larger than `n` bytes are answered with 413 |
| `h.MaxMemory(n)` | `Config.MaxMemory`, 1 MiB | multipart data held in memory; the rest goes to temporary files |
| `h.SpillAt(n)` | `Config.SpillAt`, 64 KiB | response bytes buffered before streaming |
| `h.Consumes(ranges...)` | every registered decoder | narrows the accepted `Content-Type`; others get 415 |
| `h.Produces(ranges...)` | every registered encoder | narrows what the route writes; an unmet `Accept` gets 406 |
| `h.Errors(m)` | none | adds a mapper |
| `h.ProblemFormats(fs...)` | `Config.ProblemFormats` | formats for this scope's problems |
| `h.Use(mw...)` | none | middleware for this scope |

## Set time limits

h sets no time limits of its own: your server and middleware decide them.

- **`http.Server.ReadHeaderTimeout`** bounds clients that send headers slowly.
- **`mw.Timeout(d)`** gives each request a context deadline. The endpoint sees it, and an error caused by it is answered with 503. When the server has no `ReadTimeout`, the deadline also bounds body reads, and a stalled body is answered with 408.
- **`http.Server.ReadTimeout`** caps reading the whole request. When it is set, it governs body reads instead of the request deadline. The router never lifts it.

So with a 30 s `ReadTimeout` and `mw.Timeout(5 * time.Second)`, a stalled upload is cut off at 30 s, while the endpoint's context still ends at 5 s. To bound uploads by the per-request deadline, leave `ReadTimeout` unset and rely on `ReadHeaderTimeout` for slow headers.

## Accept requests from browsers

Every route is protected against cross-site request forgery by `http.CrossOriginProtection`. Non-browser clients are unaffected. A refused request is answered with a 403 problem through the route's own mappers.

To let a frontend on another origin call the API, list its origin. The same list drives CORS:

```go
r, err := h.New(h.Config{
	TrustedOrigins: []string{"https://app.example.com"},
	CORS:           h.CORS{AllowCredentials: true},
})
```

The router never answers with `Access-Control-Allow-Origin: *` and never names an origin you did not trust.

To exempt one route that other sites must reach, such as a single sign-on callback, pass `h.InsecureAllowCrossOrigin()` to that route. Passing it to a group is a setup mistake.

## Add middleware and observe errors

`r.Use` on the root router wraps every request, including unmatched ones. On a group, it wraps that group's routes. Package `mw` provides `RequestID`, `AccessLog` and `Timeout`:

```go
r.Use(mw.RequestID("X-Request-Id"), mw.AccessLog(logger), mw.Timeout(10*time.Second))
```

`Config.OnError` receives every error the router answers, with the problem it became. An error found after the response was sent, such as failing to remove multipart temporary files, arrives with the zero `Problem`. Log there:

```go
OnError: func(ctx context.Context, req *http.Request, err error, p problem.Problem) {
	logger.ErrorContext(ctx, "request failed", "status", p.Status, "err", err)
},
```

`OnError` must not write to the response or panic.

## Mount under a base path

Set `Config.BasePath` to serve under a prefix. Routes, groups and `req.Link` are relative to it. Mount the handler in an outer mux with `r.Mount()`:

```go
mux := http.NewServeMux()
mux.Handle(r.Mount(), handler) // "/shop/" for BasePath "/shop"
```

Call `r.Fallback()` to answer unmatched paths under the router with problems: 405 with `Allow` when the path has routes for other methods, otherwise 404.

## Catch setup mistakes

Registration never panics. Every mistake is collected, and `r.Handler()` returns them all at once, each wrapping `h.ErrSetup`. Examples are a duplicate pattern, a type no codec can read or write, an invalid option, or a change made after `Handler`. `h.New` returns configuration errors directly.

Build the handler once at startup and fail fast:

```go
handler, err := r.Handler()
if err != nil {
	log.Fatal(err) // lists every mistake with its route
}
```

## Test routes and endpoints

Package `htest` sends requests to a router in process. `htest.New` fails the test when the router has setup mistakes. Paths are relative to the base path:

```go
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
```

`Do` sends a struct as JSON, and a string, `[]byte` or `io.Reader` as is. `Response.JSON[T]` decodes strictly. `Response.Problem` fails unless the body is a problem. `htest.Errors` records `OnError` calls. Use `Send` for a request you built yourself, and `httptest.NewServer` when you need a real connection.

To call an endpoint without a router, build its request with `h.NewRequest`, `h.NewFormRequest` or `h.NewRawBody`. Run a `Stream` with `Stream.WriteTo`.

## Work on h

This section is for contributors and coding agents changing the library.

### Check a change

Run these from the repository root. All three must pass:

```sh
go vet ./...
golangci-lint run ./...
go test -race ./...
```

The linter refuses `encoding/json`: use `encoding/json/v2` and `encoding/json/jsontext`.

For performance changes, compare at least ten runs with `benchstat`. Measure the old and new code one after the other on the same machine:

```sh
go test -run '^$' -bench . -count=10 . > new.txt
benchstat old.txt new.txt
```

`bench_test.go` compares the router with a hand-written handler doing the same work (`BenchmarkB1Adapter`).

### Find the code

| Package | Owns |
|---|---|
| `h` | router, endpoints, requests and responses, body reading, error resolution, CSRF and CORS |
| `problem` | the `Problem` value, mappers, wire formats, `Read` |
| `codec` | encoders, decoders and the negotiated `Set` |
| `mw` | request ID, access log, timeout middleware |
| `htest` | in-process test client; imports `testing`, so production code never imports it |
| `internal/media` | media type parsing and `Accept` negotiation (RFC 9110) |
| `internal/serverfield` | the header fields only the router may write |

Within `h`, each file owns one capability:

- `router.go`: registration, building, `Handle`, `Fallback`
- `adapter.go`: one endpoint request
- `resolve.go`: errors to problems
- `body.go`: body limits and deadlines
- `spill.go`: buffered responses
- `stream.go`: `Seq`, `Stream`, `RawBody`
- `params.go` and `form.go`: path, query, header and form values
- `cors.go`: cross-origin resource sharing

Tests sit next to the capability they check. `contracts_test.go` holds regression tests from code review, and `example_orders_test.go` is a complete worked example.

### Keep these rules

- **The library does not panic on a caller mistake.** Registration records a setup error, and request-time mistakes become problems reported to `OnError`. The panics left are `http.ErrAbortHandler`, which is net/http's documented way to abort a started response, and re-panics of it.
- **Errors keep their context.** Wrap with `%w`, and never swallow an error. A failure that cannot be answered goes to `OnError`.
- **Every router change goes through `Router.mutable`,** so a change after `Handler` is reported, not lost.
- **Whether a response has started is owned by `trackingWriter`.** Other writers read it, never copy it. `trackingWriter.answer` decides between writing a problem and aborting.

### Know the lifecycles

Each stateful value has one owner. Update this table when you add a state or a transition:

| Value | States | Owner |
|---|---|---|
| Router | registering, then built by the first `Handler` call | `core.served`, nil while registering |
| Group | usable, or broken by a bad prefix or option | `Router.broken` |
| Response | not started, then started by a final status, a body byte, a successful flush or a hijack; 1xx leaves it not started | `trackingWriter.wrote` |
| Spill writer | buffering, then streaming past `SpillAt` or on flush | derived from the response's state |
| Stream writer | open while the stream's function runs, then closed | `StreamWriter.w`, nil when closed |
| Body reader | no deadline, or pending, armed on first read, cleared at EOF; a read error is kept | `bodyReader` |
| `Seq` | unconsumed, then consumed; a second range yields `ErrConsumed` | `seqState.used` |
| Query values | unparsed, then parsed once per request | `requestState.query`, nil until parsed |
