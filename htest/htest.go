// Package htest sends requests to an h router in process and checks what it
// answers, for tests. It complements net/http/httptest, which it builds
// on: use httptest.NewServer for tests that need a real connection.
package htest

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/problem"
)

// Client sends requests to one handler and records the answers. Paths given
// to Do are relative to the router's base path.
type Client struct {
	t       testing.TB
	handler http.Handler
	base    string
}

// New returns a Client for the handler r builds, failing t if r has setup
// errors (h.Router.Handler).
func New(t testing.TB, r *h.Router) *Client {
	t.Helper()
	handler, err := r.Handler()
	if err != nil {
		t.Fatalf("htest: router setup: %v", err)
	}
	return &Client{t: t, handler: handler, base: strings.TrimSuffix(r.Mount(), "/")}
}

// ForHandler returns a Client for any handler, such as an outer ServeMux a
// router is mounted in. Paths given to Do are used as they are.
func ForHandler(t testing.TB, handler http.Handler) *Client {
	return &Client{t: t, handler: handler}
}

// RequestOption changes a request before Do sends it.
type RequestOption func(*http.Request)

// Header adds a header field to the request.
func Header(key, value string) RequestOption {
	return func(r *http.Request) { r.Header.Add(key, value) }
}

// Do sends a request for path, relative to the base path. A body of nil
// sends none; a string, []byte or io.Reader is sent as it is; any other
// value is sent as JSON with Content-Type application/json.
func (c *Client) Do(method, path string, body any, opts ...RequestOption) *Response {
	c.t.Helper()
	var r io.Reader
	isJSON := false
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	case io.Reader:
		r = b
	default:
		data, err := json.Marshal(b)
		if err != nil {
			c.t.Fatalf("htest: encoding %T: %v", body, err)
		}
		r, isJSON = bytes.NewReader(data), true
	}
	if !strings.HasPrefix(path, "/") {
		c.t.Fatalf("htest: path %q does not start with /", path)
	}
	req := httptest.NewRequest(method, c.base+path, r)
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	return c.Send(req)
}

// Send sends req as it is. A handler that aborts the response with
// http.ErrAbortHandler, as the router does when a response fails after it
// started, yields a Response with Aborted set; any other panic propagates.
func (c *Client) Send(req *http.Request) (resp *Response) {
	resp = &Response{ResponseRecorder: httptest.NewRecorder()}
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			resp.Aborted = true
		}
	}()
	c.handler.ServeHTTP(resp.ResponseRecorder, req)
	return resp
}

// Response is an answer recorded by a Client.
type Response struct {
	*httptest.ResponseRecorder
	// Aborted reports that the handler cut the connection after the
	// response started: a client would see a failed response.
	Aborted bool
}

// Expect fails t unless the response has status, showing the body.
func (r *Response) Expect(t testing.TB, status int) *Response {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status %d, want %d; body %s", r.Code, status, r.Body)
	}
	return r
}

// Problem fails t unless the body is an RFC 9457 problem
// (application/problem+json) and returns it.
func (r *Response) Problem(t testing.TB) problem.Problem {
	t.Helper()
	if ct := r.Header().Get("Content-Type"); ct != problem.RFC9457.MediaType() {
		t.Fatalf("Content-Type %q, want %s; body %s", ct, problem.RFC9457.MediaType(), r.Body)
	}
	p, err := problem.Read(bytes.NewReader(r.Body.Bytes()))
	if err != nil {
		t.Fatalf("problem body %s: %v", r.Body, err)
	}
	return p
}

// JSON decodes the body as a T, failing t when it cannot. Decoding is strict:
// unknown members and trailing data fail.
func (r *Response) JSON[T any](t testing.TB) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body.Bytes(), &v, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("decoding %T from %s: %v", v, r.Body, err)
	}
	return v
}

// Errors records OnError calls. Pass its OnError method as h.Config.OnError.
// It is safe for concurrent use.
type Errors struct {
	mu   sync.Mutex
	seen []h.Failure
}

// OnError records f.
func (e *Errors) OnError(f h.Failure) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, f)
}

// All returns the failures recorded so far, oldest first.
func (e *Errors) All() []h.Failure {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]h.Failure(nil), e.seen...)
}

// Last returns the latest failure, failing t when there was none.
func (e *Errors) Last(t testing.TB) h.Failure {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.seen) == 0 {
		t.Fatal("OnError not called")
	}
	return e.seen[len(e.seen)-1]
}
