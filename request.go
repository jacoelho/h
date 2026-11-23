package h

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

// Empty is the input of a route that reads no body and the output of one
// that writes none.
type Empty = struct{}

var emptyType = reflect.TypeFor[Empty]()

// Request is what an endpoint receives: the decoded body and the HTTP
// request it came with. The zero Request is an empty GET / request with a
// zero body. Its body has already been read, except for RawBody
// and Seq inputs, which read while the endpoint runs.
type Request[In any] struct {
	st   *requestState
	body In
}

type requestState struct {
	req      *http.Request
	base     string
	query    url.Values // nil until first read; url.ParseQuery never returns nil
	queryErr error
}

// NewRequest returns a Request carrying body, for calling an endpoint in a
// unit test. Its links are relative to "/". A nil r stands for an empty
// GET / request.
func NewRequest[In any](r *http.Request, body In) Request[In] {
	if r == nil {
		return Request[In]{body: body}
	}
	return Request[In]{st: &requestState{req: r}, body: body}
}

// HTTP returns the underlying HTTP request.
func (r Request[In]) HTTP() *http.Request { return stateOf(r.st).req }

// Body returns the decoded request body.
func (r Request[In]) Body() In { return r.body }

// Parse reads path, query and header values through f and returns nil, or a
// *ParamsError listing every value that was missing or did not parse, in
// the order f asked for them. The router answers that error with 400, or
// 404 when a path value failed. The Params is valid only inside f.
func (r Request[In]) Parse(f func(*Params)) error {
	p := &Params{st: stateOf(r.st)}
	f(p)
	return p.err()
}

// Link returns the path of a resource under the router's base path, each
// segment escaped, for Location fields and links in bodies. Segments are
// relative to the base path, not to the endpoint's group.
func (r Request[In]) Link(segments ...string) string {
	return link(stateOf(r.st).base, segments)
}

func link(base string, segments []string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// stateOf returns st, or the state of an empty GET / request for a zero
// Request or Form, so their methods work on values built by hand in tests.
func stateOf(st *requestState) *requestState {
	if st == nil {
		return &requestState{req: &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}, Header: http.Header{}}}
	}
	return st
}

func (st *requestState) queryValues() (url.Values, error) {
	if st.query == nil {
		st.query, st.queryErr = url.ParseQuery(st.req.URL.RawQuery)
	}
	return st.query, st.queryErr
}
