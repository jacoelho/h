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

- **A plain function.** Write the full signature yourself when you need the request or choose the response, such as `req.Link`, a 201 with `Location`, or a `Content-Disposition` field. `h.Func` and `h.Bind` always answer 200, or 204 for `h.Empty`.
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

To share options without a prefix, use an empty one: `r.Group("", h.Errors(OrderErrors))`.

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

For a one-off answer that no mapper needs to know, return `problem.Error(p)`. The router sends the problem as it is, after checking it is valid, without consulting mappers, even when the error is wrapped. Wrap only problems your own code made, since a problem built from another service's response is sent verbatim. Inside a refusal, such as from a custom unmarshaler, it is sent when its status is 4xx. Otherwise the router's status stands and `ErrInvalidProblem` is reported. `problem.FromError` gets it back, for tests or middleware:

```go
if stock == 0 {
	return h.Response[Order]{}, problem.Error(problem.Problem{Status: http.StatusConflict, Title: "Out of stock"})
}
```

An error no mapper knows is answered with 500. Its text goes only to `Config.OnError`, never to the client. A panic in an endpoint is answered with 500 and reported as `h.ErrPanic`. An error that wraps the request's own context error is answered by the router, without mappers: 499 when the client has gone, 503 when the request's deadline passed.

A mapper with a fixed problem is checked when it is built. Assert that in the capability's own tests:

```go
func TestOrderErrorsAreValid(t *testing.T) {
	if err := OrderErrors.Err(); err != nil {
		t.Fatal(err)
	}
}
```

A problem that `problem.As` or `problem.Func` computes is checked only at request time. If it is invalid, the router answers 500 and reports `h.ErrInvalidProblem`.

The router refuses some requests itself, with a `*h.RequestError`:

- 400, 408 or 413 for a malformed, slow or oversized body
- 415 for a `Content-Type` the route does not read
- 406 for an `Accept` it cannot satisfy
- 403 for a refused cross-origin request (`h.ErrCrossOrigin`)
- with `Fallback`, 404 (`h.ErrNoRoute`) and 405 (`h.ErrMethodNotAllowed`)

A path, query or header value that fails `Parse` is a `*h.ParamsError`, answered with 400, or 404 for a path value.

These refusals never reach `Errors` mappers, so a catch-all mapper cannot turn a 404 into a 500. Describe them with `Config.RequestErrors` or the `h.RequestErrors` option on a group or route, which work like `Errors`. A `RequestErrors` mapper may answer any 4xx status, for example 422 for a body your clients consider invalid, or 400 for a malformed path value. Any other status is reported as `h.ErrInvalidProblem`, and the router's status stands. A malformed body read by a codec, including a `Seq` element, is a `*h.RequestError` wrapping a `*codec.DecodeError`. For JSON values that error wraps json/v2's `*json.SemanticError`, whose `JSONPointer` names the field, or a `*jsontext.SyntacticError`. A malformed form is a `*h.RequestError` wrapping the `net/url` or `mime/multipart` error. The mapper is offered the refusal and what it wraps, so it can also match an error your own unmarshaler returned. For a body that is too large or too slow, it is offered the `*h.RequestError` and the cause inside it, never the decoder error around it. A body over `MaxBody` is a 413 wrapping `*http.MaxBytesError`. A multipart form too large to parse is a 413 wrapping `multipart.ErrMessageTooLarge`. A body that arrives too slowly is a 408 wrapping `os.ErrDeadlineExceeded`. Match these causes with `problem.As` or `problem.Is`. Any other failure to read the body is a 400 wrapping the server's error, which only `Status` tells apart.

List a `*codec.DecodeError` mapper before a `*h.RequestError` mapper, since a malformed body matches both:

```go
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

r, err := h.New(h.Config{RequestErrors: RefusalProblems})
```

Problems are written as `application/problem+json` by default. To keep an error shape your clients already read, set `Config.ProblemFormats`, or use the `h.ProblemFormats` option on a group, with `problem.JSON(mediaType, shape)`. The last format listed answers a client whose `Accept` matches none, as RFC 9110 allows. The default list is `problem.RFC9457` then `problem.Text`. A list you set replaces it, so `[]problem.Format{problem.RFC9457}` answers every client with JSON. `Annotate`, the format and `OnError` receive the status's reason phrase as the title of an `about:blank` problem that has none. `problem.Read` decodes a problem body, for clients and tests.

## Read forms, uploads and sequences

The type of `In` selects how the body is read:

| `In` | Body | Notes |
|---|---|---|
| a struct or other value | decoded by a codec chosen from `Content-Type` | JSON and `text/plain` by default |
| `h.Form` | `application/x-www-form-urlencoded` or `multipart/form-data` | read values with `req.Body().Parse`; temporary files are removed after the request |
| `h.RawBody` | the bytes as sent | the route must set `h.Consumes`; `"*/*"` accepts any valid `Content-Type`, or none |
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

An endpoint taking `h.RawBody` or `h.Seq[T]` may keep reading the body after its response has started, such as when it streams one output element per input element. Reading the body is bounded by `MaxBody`. A body that is malformed, too large or too slow is answered with 400, 413 or 408. For `h.RawBody` and `h.Seq[T]`, which the endpoint reads itself, that happens when the endpoint returns the read error.

A body sent without `Content-Type` is matched as `application/octet-stream`, as RFC 9110 allows. `RawBody.ContentType` still returns the empty string, so the endpoint can tell that the client named no type.

## Stream a response

Return `h.Seq[T]` to write a JSON array or NDJSON element by element. The router ranges over the source after the endpoint returns, so the source must honour its context:

```go
orders.Get("/export", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[Order]], error) {
	return h.OK(h.Items(svc.All(ctx))), nil
})
```

NDJSON is not in the default codec set. Register it with `codec.Set.Encodes` and `Decodes`, and pass the set as `Config.Codecs`. Another JSON media type works the same way. This download is JSON-LD with object members in a fixed order:

```go
codecs := codec.Default(codec.JSON{})
ld := codec.JSON{Options: []json.Options{json.Deterministic(true)}}
if err := codecs.Encodes(ld, "application/ld+json"); err != nil {
	log.Fatal(err)
}

r.Get("/orders.jsonld", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[Order]], error) {
	return h.OK(h.Items(svc.All(ctx))).
		WithHeader("Content-Disposition", `attachment; filename="orders.jsonld"`), nil
}, h.Produces("application/ld+json"))
```

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

For a handler that must write the response itself, such as one serving files, use `Handle`. An error it returns before writing is answered as a problem:

```go
r.Handle("GET", "/static/{file...}", func(w http.ResponseWriter, req *http.Request) error {
	http.ServeFileFS(w, req, os.DirFS("static"), req.PathValue("file"))
	return nil
})
```

`h.Raw` adapts a handler that returns no error, either a plain function or an `http.Handler` passed as its method value:

```go
r.Handle("GET", "/healthz", h.Raw(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}))
r.Handle("PROPFIND", "/dav/{path...}", h.Raw(davHandler.ServeHTTP))
```

`http.FileServer` looks up the whole request path, base path included, so wrap it in `http.StripPrefix` with the mounted prefix before passing it.

## Set limits and media types

Options apply to a group or a single route. The closest setting wins:

| Option | Default | Effect |
|---|---|---|
| `h.MaxBody(n)` | `Config.MaxBody`, 1 MiB | request bodies larger than `n` bytes are answered with 413 |
| `h.MaxMemory(n)` | `Config.MaxMemory`, 1 MiB | multipart data held in memory; the rest goes to temporary files |
| `h.SpillAt(n)` | `Config.SpillAt`, 64 KiB | response bytes buffered before streaming |
| `h.Consumes(ranges...)` | every registered decoder | narrows the accepted `Content-Type`; others get 415 |
| `h.Produces(ranges...)` | every registered encoder | narrows what the route writes; an unmet `Accept` gets 406 |
| `h.Errors(m)` | none | adds a mapper for the errors endpoints return |
| `h.RequestErrors(m)` | none | adds a mapper for the requests the router refuses |
| `h.ProblemFormats(fs...)` | `Config.ProblemFormats` | formats for this scope's problems |
| `h.Use(mw...)` | none | middleware for this scope |

`Config.Codecs` holds the codecs every route negotiates between, `codec.Default(codec.JSON{})` when nil. To refuse JSON objects with members no field matches, pass `codec.Default(codec.JSON{RejectUnknownFields: true})`. The same codec writes responses, so its `Options` apply to both directions.

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

## Add middleware, request data and logging

`r.Use` on the root router wraps every request, including unmatched ones. On a group, it wraps that group's routes. Package `mw` provides `RequestID`, `AccessLog` and `Timeout`. `AccessLog` writes one line per request to the logger you pass, at the level you pass:

```go
r.Use(mw.RequestID("X-Request-Id"), mw.AccessLog(logger, slog.LevelInfo), mw.Timeout(10*time.Second))
```

Every problem the router answers is described by an `h.Failure`: the `Request`, the `Err` that caused it, and the `Problem` the client is sent. Two hooks receive it.

`Config.Annotate` adds request data to every problem, including the ones the router raises itself, such as a 404 or a malformed body. It runs after the mappers, so they stay functions of the error alone. It must keep the status. `Problem.With` adds a member without changing a map another problem shares:

```go
Annotate: func(f h.Failure) problem.Problem {
	if id, ok := mw.RequestIDFrom(f.Request.Context()); ok {
		return f.Problem.With("request_id", id)
	}
	return f.Problem
},
```

`Config.OnError` observes every failure and gets its own copy of the problem. An error found after the response was sent, such as failing to remove multipart temporary files, arrives with the zero `Problem`. Log there:

```go
OnError: func(f h.Failure) {
	logger.ErrorContext(f.Request.Context(), "request failed", "status", f.Problem.Status, "err", f.Err)
},
```

`OnError` must not write to the response or panic.

## Mount under a base path

Set `Config.BasePath` to serve under a prefix. Routes, groups and `req.Link` are relative to it. Mount the handler in an outer mux with `r.Mount()`:

```go
mux := http.NewServeMux()
mux.Handle(r.Mount(), handler) // "/shop/" for BasePath "/shop"
```

Call `r.Fallback()` to answer unmatched paths under the router with problems: 405 with `Allow` when the path has routes for other methods, otherwise 404. Without it, those requests get net/http's plain-text answers.

To serve a single-page app under the same base path as the API, register it as a catch-all route. The mux picks the most specific pattern, so API routes still win. The catch-all also answers unknown API paths: a GET gets the app, and other methods get 405. To answer those with 404, serve the app under its own prefix, such as `/app/{path...}`:

```go
r.Get("/api/orders", listOrders)
r.Handle("GET", "/{path...}", h.Raw(app.ServeHTTP)) // every other GET, such as /shop/recipes/42
```

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

`Do` sends a struct as JSON, and a string, `[]byte` or `io.Reader` as is. `Response.JSON[T]` decodes strictly. `Response.Problem` fails unless the body is an `application/problem+json` problem. For a custom problem format, decode the body with `Response.JSON[T]` and your error type. `htest.Errors` records each `h.Failure` passed to `OnError`. When the router aborts a response that failed after it started, `Response.Aborted` is set. Use `Send` for a request you built yourself, and `httptest.NewServer` when you need a real connection.

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
