package h

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jacoelho/h/codec"
)

// Errors the router reports to OnError. Endpoints never need to return them.
var (
	// ErrUnsupportedMediaType reports a request body in a media type the
	// route does not read (415).
	ErrUnsupportedMediaType = codec.ErrUnsupportedMediaType
	// ErrNotAcceptable reports a request whose Accept field admits none of
	// the media types the route writes (406).
	ErrNotAcceptable = codec.ErrNotAcceptable
	// ErrInvalidResponse reports a Response that cannot be written, such as
	// a body with status 204 or a Content-Type set by the endpoint.
	ErrInvalidResponse = errors.New("h: invalid response")
	// ErrInvalidProblem reports a problem the router discarded: a mapper,
	// Annotate or problem.Error answer that is invalid, a RequestErrors
	// answer outside 4xx, an Annotate answer that changes the status, or a
	// RequestError with a status outside 400 to 599.
	ErrInvalidProblem = errors.New("h: invalid problem")
	// ErrPanic reports an endpoint that panicked; see PanicError.
	ErrPanic = errors.New("h: endpoint panicked")
	// ErrCrossOrigin reports an unsafe cross-origin browser request refused
	// by cross-site request forgery protection (403).
	ErrCrossOrigin = errors.New("h: cross-origin request refused")
	// ErrEncodeAfterCommit reports a failure after the response status was
	// sent; the connection is aborted.
	ErrEncodeAfterCommit = errors.New("h: response failed after it started")
	// ErrProblemFormat reports a problem format that failed to write; the
	// problem is written as text instead.
	ErrProblemFormat = errors.New("h: problem format failed")
	// ErrMissing reports a required parameter that the request lacks.
	ErrMissing = errors.New("h: missing")
	// ErrConsumed reports a second pass over a single-pass Seq.
	ErrConsumed = errors.New("h: sequence already consumed")
)

// RequestError is a request the router refuses with a fixed status: a body
// that is too large (413), malformed (400) or too slow (408), or in a media
// type the route does not read (415), an Accept field it cannot satisfy
// (406), a refused cross-origin request (403, ErrCrossOrigin), or, with
// Fallback, a path no route matches (404, ErrNoRoute) or a method it does
// not serve (405, ErrMethodNotAllowed). The router returns it from the boundaries it owns, such as the body
// reader, so an error the endpoint produces itself is never mistaken for
// one. RequestErrors mappers describe it and may choose another 4xx
// status; they are offered the RequestError and what it wraps, while errors
// wrapped around it, by a decoder or an endpoint, are reported to OnError
// but not matched. Errors mappers are never offered it.
type RequestError struct {
	Status int
	Err    error
}

// Error describes the refused request and its cause.
func (e *RequestError) Error() string {
	return fmt.Sprintf("h: request refused with %d %s: %v", e.Status, http.StatusText(e.Status), e.Err)
}

// Unwrap returns the cause.
func (e *RequestError) Unwrap() error { return e.Err }

// PanicError is an endpoint panic, reported to OnError. It matches ErrPanic.
type PanicError struct {
	Value any
	Stack []byte
}

// Error describes the panic value.
func (e *PanicError) Error() string { return fmt.Sprintf("h: endpoint panicked: %v", e.Value) }

// Unwrap returns ErrPanic, so errors.Is(err, ErrPanic) matches.
func (e *PanicError) Unwrap() error { return ErrPanic }

// sourceError is an error a Seq's source iterator yielded while the
// response was being encoded, as opposed to a failure of the encoder.
type sourceError struct{ err error }

func (e *sourceError) Error() string { return e.err.Error() }

func (e *sourceError) Unwrap() error { return e.err }

func requestFault(err error, status int) error {
	if _, ok := errors.AsType[*RequestError](err); ok {
		return err
	}
	return &RequestError{Status: status, Err: err}
}

// decodeFault classifies a decoder's error: a body read failure keeps its
// status, malformed input is 400, and anything else is a codec failure.
func decodeFault(err error) error {
	if _, ok := errors.AsType[*RequestError](err); ok {
		return err
	}
	if _, ok := errors.AsType[*codec.DecodeError](err); ok {
		return &RequestError{Status: http.StatusBadRequest, Err: err}
	}
	return err
}
