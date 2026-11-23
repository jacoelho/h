package h

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jacoelho/h/problem"
)

// Params reads path, query and header values inside Request.Parse, parsing
// each into its type and recording every failure. On failure an accessor
// returns the zero value; Parse reports the failures together.
type Params struct {
	st   *requestState
	errs []ParamError
}

// ParamsError lists the parameters a request lacked or that did not parse.
// It wraps each parameter's Err in order. A parser may return
// problem.Error(p) to answer with p in place of the router's 400 or 404;
// the first parameter carrying one wins, and p must have a 4xx status.
type ParamsError struct {
	Params []ParamError
}

// ParamError is one parameter that was missing (Err is ErrMissing) or did
// not parse (Err is the parser's error).
type ParamError struct {
	// In is where the parameter was: path, query, header, form or file.
	In   string
	Name string
	Err  error
}

// Error lists each parameter that was missing or malformed.
func (e *ParamsError) Error() string {
	parts := make([]string, len(e.Params))
	for i, p := range e.Params {
		parts[i] = fmt.Sprintf("%s %s: %v", p.In, p.Name, p.Err)
	}
	return "h: invalid parameters: " + strings.Join(parts, "; ")
}

// Unwrap returns each parameter's Err, in the order they were asked for.
func (e *ParamsError) Unwrap() []error {
	errs := make([]error, len(e.Params))
	for i, p := range e.Params {
		errs[i] = p.Err
	}
	return errs
}

func (e *ParamsError) problem() problem.Problem {
	if slices.ContainsFunc(e.Params, func(p ParamError) bool { return p.In == "path" }) {
		return problem.Problem{Status: http.StatusNotFound}
	}
	list := make([]map[string]string, len(e.Params))
	for i, p := range e.Params {
		reason := "invalid"
		if errors.Is(p.Err, ErrMissing) {
			reason = "required"
		}
		list[i] = map[string]string{"in": p.In, "name": p.Name, "reason": reason}
	}
	return problem.Problem{Status: http.StatusBadRequest, Extra: map[string]any{"invalid_params": list}}
}

func (p *Params) fail(in, name string, err error) {
	p.errs = append(p.errs, ParamError{In: in, Name: name, Err: err})
}

func (p *Params) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return &ParamsError{Params: p.errs}
}

func first[T any](p *Params, in, name string, vs []string, parse func(string) (T, error)) (T, bool) {
	if len(vs) == 0 {
		var zero T
		return zero, false
	}
	return parseOne(p, in, name, vs[0], parse), true
}

func required[T any](p *Params, in, name string, vs []string, parse func(string) (T, error)) T {
	v, ok := first(p, in, name, vs, parse)
	if !ok {
		p.fail(in, name, ErrMissing)
	}
	return v
}

func orDefault[T any](p *Params, in, name string, vs []string, def T, parse func(string) (T, error)) T {
	if v, ok := first(p, in, name, vs, parse); ok {
		return v
	}
	return def
}

func parseOne[T any](p *Params, in, name, raw string, parse func(string) (T, error)) T {
	v, err := parse(raw)
	if err != nil {
		p.fail(in, name, err)
		var zero T
		return zero
	}
	return v
}

// Path returns the path wildcard name parsed by parse. A path value that
// does not parse makes the request answer 404, since the path then names no
// resource. A remainder wildcard such as {rest...} may be empty, and parse
// then receives "".
func (p *Params) Path[T any](name string, parse func(string) (T, error)) T {
	raw := p.st.req.PathValue(name)
	if raw == "" && !strings.Contains(p.st.req.Pattern, "{"+name+"...}") {
		p.fail("path", name, ErrMissing)
		var zero T
		return zero
	}
	return parseOne(p, "path", name, raw, parse)
}

func (p *Params) queryAll(name string) []string {
	q, err := p.st.queryValues()
	if err != nil && !slices.ContainsFunc(p.errs, func(e ParamError) bool { return e.In == "query" && e.Name == "" }) {
		p.fail("query", "", err)
	}
	return q[name]
}

// Query returns the first value of the query parameter name parsed by
// parse, recording ErrMissing when it is absent. An empty value is present
// and passed to parse.
func (p *Params) Query[T any](name string, parse func(string) (T, error)) T {
	return required(p, "query", name, p.queryAll(name), parse)
}

// QueryOr returns the first value of the query parameter name parsed by
// parse, or def when it is absent.
func (p *Params) QueryOr[T any](name string, def T, parse func(string) (T, error)) T {
	return orDefault(p, "query", name, p.queryAll(name), def, parse)
}

// QueryOpt returns the first value of the query parameter name parsed by
// parse, and whether it was present.
func (p *Params) QueryOpt[T any](name string, parse func(string) (T, error)) (T, bool) {
	return first(p, "query", name, p.queryAll(name), parse)
}

// QueryAll returns every value of the query parameter name, in order, each
// parsed by parse.
func (p *Params) QueryAll[T any](name string, parse func(string) (T, error)) []T {
	return parseEach(p, "query", name, p.queryAll(name), parse)
}

func parseEach[T any](p *Params, in, name string, vs []string, parse func(string) (T, error)) []T {
	out := make([]T, 0, len(vs))
	for _, v := range vs {
		out = append(out, parseOne(p, in, name, v, parse))
	}
	return out
}

// Header returns the first value of the header field name parsed by parse,
// recording ErrMissing when it is absent.
func (p *Params) Header[T any](name string, parse func(string) (T, error)) T {
	return required(p, "header", name, p.st.req.Header.Values(name), parse)
}

// HeaderOr returns the first value of the header field name parsed by
// parse, or def when it is absent.
func (p *Params) HeaderOr[T any](name string, def T, parse func(string) (T, error)) T {
	return orDefault(p, "header", name, p.st.req.Header.Values(name), def, parse)
}

// HeaderOpt returns the first value of the header field name parsed by
// parse, and whether it was present.
func (p *Params) HeaderOpt[T any](name string, parse func(string) (T, error)) (T, bool) {
	return first(p, "header", name, p.st.req.Header.Values(name), parse)
}

// String returns s unchanged: the parser for text parameters.
func String(s string) (string, error) { return s, nil }

// Int64 parses a base 10 integer.
func Int64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// Time returns a parser for times in layout.
func Time(layout string) func(string) (time.Time, error) {
	return func(s string) (time.Time, error) { return time.Parse(layout, s) }
}

// OneOf returns a parser accepting exactly one of allowed.
func OneOf[T ~string](allowed ...T) func(string) (T, error) {
	return func(s string) (T, error) {
		if slices.Contains(allowed, T(s)) {
			return T(s), nil
		}
		return "", fmt.Errorf("%q is not one of %q", s, allowed)
	}
}
