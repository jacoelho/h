package h

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jacoelho/h/internal/media"
	"github.com/jacoelho/h/problem"
)

// Failure is a request the router answers with a problem, as Config.Annotate
// and Config.OnError receive it.
type Failure struct {
	// Request is the request that failed. Its context may already be done.
	Request *http.Request
	// Err is what went wrong, for logs. It is never sent to the client.
	Err error
	// Problem is what the client is sent. It is the zero Problem for an
	// error found after the response started, such as an encoding failure
	// that aborts the connection or failing to remove multipart temporary
	// files, since nothing more is sent. When the router answers 499 because
	// the request's context was canceled, only the status is written; a 499
	// a mapper or an endpoint chose is written in full.
	Problem problem.Problem
}

// Hidden reports whether the problem the client got, or its absence, does
// not show the failure, so a policy keyed on Problem.Status would miss it:
//   - a panic (ErrPanic), wherever it happened and whatever was answered;
//   - a mapper or Annotate answer the router discarded (ErrInvalidProblem),
//     sent with the router's own status;
//   - a problem that could not be written in the negotiated format
//     (ErrProblemFormat);
//   - multipart temporary files that could not be removed;
//   - a response that failed after it started (ErrEncodeAfterCommit),
//     including one cut by the server's WriteTimeout, unless the failure
//     is the connection's because the client went away: the reported error
//     is the failed write or flush to the client, or the request's canceled
//     context.
//
// A 499 answered before the response started is not hidden, though over
// HTTP/2 it may be the server's WriteTimeout, which cancels the request's
// context by resetting the stream, rather than the client going away.
//
// Hidden depends only on Err, so it gives the same answer after the request
// ends. In Annotate, which runs before the problem is written, only a
// discarded mapper answer or a mapper's panic is known.
func (f Failure) Hidden() bool {
	switch {
	case errors.Is(f.Err, ErrPanic), errors.Is(f.Err, ErrInvalidProblem), errors.Is(f.Err, ErrProblemFormat), errors.Is(f.Err, errMultipartCleanup):
		return true
	case errors.Is(f.Err, ErrEncodeAfterCommit):
		return !errors.Is(f.Err, errConnectionLost)
	}
	return false
}

const statusClientClosedRequest = 499

// mapping holds a scope's mappers in the order they are tried: domain
// mappers for the errors endpoints return, and refusal mappers for the
// requests the router refuses itself.
type mapping struct {
	domain   []problem.Mapper
	refusals []problem.Mapper
}

// resolver turns errors into problems for one route or scope: the mappers,
// the formats to write problems in, and the observer.
type resolver struct {
	annotate func(Failure) problem.Problem
	onError  func(Failure)
	mapping
	formats []problem.Format
	offers  []media.Type
}

func newResolver(m mapping, formats []problem.Format, annotate func(Failure) problem.Problem, onError func(Failure)) (*resolver, error) {
	if len(formats) == 0 {
		return nil, errors.New("h: no problem formats")
	}
	fs := slices.Clone(formats)
	offers := make([]media.Type, len(fs))
	for i, f := range fs {
		if f == nil {
			return nil, errors.New("h: nil problem format")
		}
		t, ok := media.Parse(f.MediaType())
		if !ok || !t.Concrete() {
			return nil, fmt.Errorf("h: problem format media type %q is not a concrete media type", f.MediaType())
		}
		offers[i] = t
	}
	return &resolver{mapping: m, formats: fs, offers: offers, annotate: annotate, onError: onError}, nil
}

// mapped asks mappers in order. A mapper that panics answers nothing, and
// its panic is returned as a *PanicError for the caller to report.
func mapped(mappers []problem.Mapper, err error) (p problem.Problem, ok bool, perr error) {
	defer func() {
		if v := recover(); v != nil {
			p, ok, perr = problem.Problem{}, false, panicError(v)
		}
	}()
	for _, m := range mappers {
		if p, ok := m.Problem(err); ok {
			return p, true, nil
		}
	}
	return problem.Problem{}, false, nil
}

// resolve returns the problem for err, whether the answer is the 499 for a
// canceled request context, and the error to report, which adds
// ErrInvalidProblem when a mapper's answer was discarded. A problem of type
// about:blank without a title gets the status's reason phrase, so Annotate,
// formats and OnError see the title the client reads.
func (rs *resolver) resolve(req *http.Request, err error) (p problem.Problem, canceled bool, report error) {
	p, canceled, report = rs.classify(req, err)
	p.Title = p.TitleOrStatus()
	p, report = rs.annotated(req, report, p)
	return p, canceled, report
}

// annotated lets Config.Annotate add request data to p. An answer that
// changes the status or is invalid, or a panic, is reported and p stands.
func (rs *resolver) annotated(req *http.Request, report error, p problem.Problem) (q problem.Problem, rerr error) {
	if rs.annotate == nil {
		return p, report
	}
	defer func() {
		if v := recover(); v != nil {
			q, rerr = p, errors.Join(report, panicError(v))
		}
	}()
	q = rs.annotate(Failure{Request: req, Err: report, Problem: clone(p)})
	if q.Status != p.Status {
		return p, errors.Join(report, fmt.Errorf("%w: Annotate changed status %d to %d", ErrInvalidProblem, p.Status, q.Status))
	}
	if verr := q.Validate(); verr != nil {
		return p, errors.Join(report, fmt.Errorf("%w: Annotate: %w", ErrInvalidProblem, verr))
	}
	q.Title = q.TitleOrStatus()
	return q, report
}

// clone copies p's header and extension maps, so a hook that adds or
// removes entries cannot change another holder's problem. Values inside
// them, such as the invalid_params list, are shared.
func clone(p problem.Problem) problem.Problem {
	p.Header = p.Header.Clone()
	p.Extra = maps.Clone(p.Extra)
	return p
}

// classify chooses the problem for err. Refusals come first, so a carried
// problem inside one, such as from a custom unmarshaler, is held to the
// refusal's 4xx range. An error that wraps the request's ended context is
// answered before mappers are consulted, so a catch-all mapper cannot turn
// it into a 500; one that does not is the endpoint's to describe, and is
// answered for the ended context only when no mapper knows it.
func (rs *resolver) classify(req *http.Request, err error) (problem.Problem, bool, error) {
	if pe, ok := errors.AsType[*ParamsError](err); ok {
		p, report := rs.refuse(err, err, pe.problem())
		return p, false, report
	}
	if re, ok := errors.AsType[*RequestError](err); ok {
		if re.Status < 400 || re.Status > 599 {
			return internalProblem(), false, errors.Join(err, fmt.Errorf("%w: RequestError status %d", ErrInvalidProblem, re.Status))
		}
		p, report := rs.refuse(err, re, problem.Problem{Status: re.Status})
		return p, false, report
	}
	if p, ok := problem.FromError(err); ok {
		if verr := p.Validate(); verr != nil {
			return internalProblem(), false, errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
		}
		return p, false, err
	}
	if cause := req.Context().Err(); cause != nil && errors.Is(err, cause) {
		p, canceled := ended(cause)
		return p, canceled, err
	}
	p, ok, perr := mapped(rs.domain, err)
	if perr != nil {
		return internalProblem(), false, errors.Join(err, perr)
	}
	if ok {
		if verr := p.Validate(); verr != nil {
			return internalProblem(), false, errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
		}
		return p, false, err
	}
	if cause := req.Context().Err(); cause != nil {
		p, canceled := ended(cause)
		return p, canceled, err
	}
	return internalProblem(), false, err
}

// refuse answers a request the router refuses with the problem carried in
// cause, or else the refusal mappers' answer, and reports err, the chain it
// arrived in. For a RequestError, cause is the RequestError and what it
// wraps, not the chain around it, such as a decoder wrapping a body read
// failure. Either answer may have any 4xx status; otherwise fixed stands.
func (rs *resolver) refuse(err, cause error, fixed problem.Problem) (problem.Problem, error) {
	p, ok := problem.FromError(cause)
	var perr error
	if !ok {
		p, ok, perr = mapped(rs.refusals, cause)
	}
	if perr != nil {
		return fixed, errors.Join(err, perr)
	}
	if !ok {
		return fixed, err
	}
	if p.Status < 400 || p.Status > 499 {
		return fixed, errors.Join(err, fmt.Errorf("%w: status %d for a refused request; want 4xx", ErrInvalidProblem, p.Status))
	}
	if verr := p.Validate(); verr != nil {
		return fixed, errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
	}
	return p, err
}

// ended answers a request whose context ended with cause: 499 when it was
// canceled, and 503 when the deadline passed.
//
// A canceled context usually means the client went away, but over HTTP/2
// the server's WriteTimeout also cancels it by resetting the stream. Before
// the response starts nothing tells the two apart: the flush that
// trackingWriter.connectionLost probes with would send the response.
func ended(cause error) (p problem.Problem, canceled bool) {
	if errors.Is(cause, context.Canceled) {
		return problem.Problem{Status: statusClientClosedRequest}, true
	}
	return problem.Problem{Status: http.StatusServiceUnavailable}, false
}

func internalProblem() problem.Problem {
	return problem.Problem{Status: http.StatusInternalServerError}
}

// fail answers err as a problem. The response must not have started.
func (rs *resolver) fail(w http.ResponseWriter, req *http.Request, err error) {
	p, canceled, report := rs.resolve(req, err)
	rs.observe(req, report, p)
	rs.write(w, req, p, canceled)
}

func (rs *resolver) observe(req *http.Request, err error, p problem.Problem) {
	if rs.onError != nil {
		rs.onError(Failure{Request: req, Err: err, Problem: clone(p)})
	}
}

// write sends p. For a canceled request context it sends only the status,
// since nothing more would reach a client that went away. A body that fails
// to write is a response that failed after it started: it is reported and
// the connection is aborted.
func (rs *resolver) write(w http.ResponseWriter, req *http.Request, p problem.Problem, canceled bool) {
	tw, ok := w.(*trackingWriter)
	if !ok {
		tw = &trackingWriter{ResponseWriter: w}
		tw.inherit()
	}
	h := tw.Header()
	h.Del("Content-Length")
	for i, k := range representationFields {
		if tw.inherited&(1<<i) == 0 {
			h.Del(k)
		}
	}
	if canceled {
		tw.WriteHeader(p.Status)
		return
	}
	i, ok := media.Negotiate(req.Header.Values("Accept"), rs.offers, true)
	if !ok {
		// RFC 9110, section 12.4.1: a server may disregard an Accept it
		// cannot satisfy.
		i = len(rs.formats) - 1
	}
	var buf bytes.Buffer
	f := rs.formats[i]
	if err := writeFormat(f, &buf, p); err != nil {
		rs.observe(req, fmt.Errorf("%w: %s: %w", ErrProblemFormat, f.MediaType(), err), p)
		buf.Reset()
		f = problem.Text
		// Text writes to a bytes.Buffer, which cannot fail; its doc
		// promises it never fails for a valid Problem.
		_ = f.Write(&buf, p) //nolint:errcheck // Text cannot fail for a valid Problem; see above.
	}
	for k, vs := range p.Header {
		k = http.CanonicalHeaderKey(k)
		if k == "Vary" {
			for _, v := range vs {
				addVary(h, v)
			}
			continue
		}
		h[k] = slices.Clone(vs)
	}
	h.Set("Content-Type", f.MediaType())
	addVary(h, "Accept")
	tw.WriteHeader(p.Status)
	if _, err := tw.Write(buf.Bytes()); err != nil {
		abort(req, rs, err, tw.connectionLost(req, err))
	}
}

// representationFields describe a response body. A problem replaces the
// body a route meant to send, so the fields the route set would describe
// the wrong one. Fields set before the route was entered are kept: they
// come from middleware that also handles the problem's body, which is why
// http.Error keeps Content-Encoding (go.dev/issue/66343). At most eight,
// to fit trackingWriter.inherited.
var representationFields = []string{"Content-Encoding", "Content-Range", "Content-Disposition", "ETag", "Last-Modified", "Cache-Control"}

// addVary adds each field name in value to the Vary field unless present.
func addVary(h http.Header, value string) {
	present := map[string]bool{}
	for _, v := range h.Values("Vary") {
		for f := range strings.SplitSeq(v, ",") {
			present[strings.ToLower(strings.TrimSpace(f))] = true
		}
	}
	for f := range strings.SplitSeq(value, ",") {
		f = strings.TrimSpace(f)
		if f != "" && !present[strings.ToLower(f)] {
			h.Add("Vary", f)
			present[strings.ToLower(f)] = true
		}
	}
}

// writeFormat runs a format, turning its panic into an error so the
// problem can still be written as text.
func writeFormat(f problem.Format, buf *bytes.Buffer, p problem.Problem) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = panicError(v)
		}
	}()
	return f.Write(buf, p)
}
