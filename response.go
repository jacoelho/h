package h

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/jacoelho/h/internal/serverfield"
)

// Response is what an endpoint answers with: a status, header fields and a
// body. Build it with OK, Created, Accepted, NoContent, NotModified or
// Status. The zero Response answers 200, or 204 when Out is Empty; with Out
// a Seq it is an empty sequence.
type Response[Out any] struct {
	status   int
	explicit bool // status was chosen with Status, so 0 is a mistake, not the default
	header   []headerField
	body     Out
	bodiless bool
}

type headerField struct{ key, value string }

// OK answers 200 with v.
func OK[T any](v T) Response[T] { return Response[T]{status: http.StatusOK, body: v} }

// Created answers 201 with v and a Location field naming the new resource.
// For a 201 without Location, use Status(http.StatusCreated, v).
func Created[T any](location string, v T) Response[T] {
	return Response[T]{status: http.StatusCreated, body: v}.WithHeader("Location", location)
}

// Accepted answers 202 with v.
func Accepted[T any](v T) Response[T] { return Response[T]{status: http.StatusAccepted, body: v} }

// NoContent answers 204 with no body.
func NoContent[T any]() Response[T] { return Response[T]{status: http.StatusNoContent, bodiless: true} }

// NotModified answers 304 with no body.
func NotModified[T any]() Response[T] {
	return Response[T]{status: http.StatusNotModified, bodiless: true}
}

// Status answers code with v. Codes that forbid a body (1xx, 204, 304) and
// codes outside 200 to 599 are refused when the response is written: use
// NoContent or NotModified.
func Status[T any](code int, v T) Response[T] {
	return Response[T]{status: code, explicit: true, body: v}
}

// WithHeader returns a copy of r with a header field added. Content-Type,
// Content-Length and the Access-Control-* fields belong to the router and
// are refused when the response is written. The response's values for a
// field replace any that middleware set earlier, except Vary, whose values
// are merged with the router's own.
func (r Response[T]) WithHeader(key, value string) Response[T] {
	r.header = append(slices.Clip(r.header), headerField{http.CanonicalHeaderKey(key), value})
	return r
}

// Status returns the status the response is written with.
func (r Response[T]) Status() int {
	if r.status != 0 {
		return r.status
	}
	if reflect.TypeFor[T]() == emptyType {
		return http.StatusNoContent
	}
	return http.StatusOK
}

// Header returns a copy of the header fields the response adds.
func (r Response[T]) Header() http.Header {
	h := http.Header{}
	for _, f := range r.header {
		h.Add(f.key, f.value)
	}
	return h
}

// Body returns the response body.
func (r Response[T]) Body() T { return r.body }

func (r Response[T]) validate(outEmpty bool) error {
	for _, f := range r.header {
		if serverfield.Owned(f.key) {
			return fmt.Errorf("%w: header %s belongs to the router", ErrInvalidResponse, f.key)
		}
	}
	if r.bodiless || (r.status == 0 && !r.explicit) {
		return nil
	}
	switch {
	case r.status < 200 || r.status > 599:
		return fmt.Errorf("%w: status %d", ErrInvalidResponse, r.status)
	case r.status == http.StatusNoContent || r.status == http.StatusNotModified:
		if !outEmpty {
			return fmt.Errorf("%w: status %d cannot carry a body; use NoContent or NotModified", ErrInvalidResponse, r.status)
		}
	}
	return nil
}

func applyHeader(h http.Header, fields []headerField) {
	var set map[string]bool
	for _, f := range fields {
		if f.key == "Vary" {
			addVary(h, f.value)
			continue
		}
		if !set[f.key] {
			h.Del(f.key)
			if set == nil {
				set = map[string]bool{}
			}
			set[f.key] = true
		}
		h.Add(f.key, f.value)
	}
}
