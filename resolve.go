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
	// files, since nothing more is sent.
	Problem problem.Problem
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
	mapping
	formats  []problem.Format
	offers   []media.Type
	annotate func(Failure) problem.Problem
	onError  func(Failure)
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

// resolve returns the problem for err and the error to report, which adds
// ErrInvalidProblem when a mapper's answer was discarded. A problem of type
// about:blank without a title gets the status's reason phrase, so Annotate,
// formats and OnError see the title the client reads.
func (rs *resolver) resolve(req *http.Request, err error) (problem.Problem, error) {
	p, report := rs.classify(req, err)
	p.Title = p.TitleOrStatus()
	return rs.annotated(req, report, p)
}

// annotated lets Config.Annotate add request data to p. An answer that
// changes the status or is invalid, or a panic, is reported and p stands.
func (rs *resolver) annotated(req *http.Request, report error, p problem.Problem) (q problem.Problem, _ error) {
	if rs.annotate == nil {
		return p, report
	}
	defer func() {
		if v := recover(); v != nil {
			q, report = p, errors.Join(report, panicError(v))
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
// problem inside one, such as from a custom unmarshaler, is for the
// RequestErrors mappers to use and cannot leave the 4xx range.
func (rs *resolver) classify(req *http.Request, err error) (problem.Problem, error) {
	if pe, ok := errors.AsType[*ParamsError](err); ok {
		return rs.refuse(err, err, pe.problem())
	}
	if re, ok := errors.AsType[*RequestError](err); ok {
		if re.Status < 400 || re.Status > 599 {
			return internalProblem(), errors.Join(err, fmt.Errorf("%w: RequestError status %d", ErrInvalidProblem, re.Status))
		}
		return rs.refuse(err, re, problem.Problem{Status: re.Status})
	}
	if p, ok := problem.FromError(err); ok {
		if verr := p.Validate(); verr != nil {
			return internalProblem(), errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
		}
		return p, err
	}
	if p, ok := cancelled(req, err); ok {
		return p, err
	}
	p, ok, perr := mapped(rs.domain, err)
	if perr != nil {
		return internalProblem(), errors.Join(err, perr)
	}
	if ok {
		if verr := p.Validate(); verr != nil {
			return internalProblem(), errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
		}
		return p, err
	}
	switch req.Context().Err() {
	case context.Canceled:
		return problem.Problem{Status: statusClientClosedRequest}, err
	case context.DeadlineExceeded:
		return problem.Problem{Status: http.StatusServiceUnavailable}, err
	}
	return internalProblem(), err
}

// refuse answers a request the router refuses, offering cause to the
// refusal mappers and reporting err, the chain it arrived in. For a
// RequestError, cause is the RequestError and what it wraps, not the chain
// around it, such as a decoder wrapping a body read failure. A mapper may
// answer any 4xx status; otherwise fixed stands.
func (rs *resolver) refuse(err, cause error, fixed problem.Problem) (problem.Problem, error) {
	p, ok, perr := mapped(rs.refusals, cause)
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

// cancelled answers an error caused by the request's own context ending:
// 499 when the client went away, 503 when the deadline passed. Mappers are
// not consulted, so a catch-all mapper cannot turn either into a 500. An
// error that does not wrap the context's error, such as a not-found found
// just before the deadline, is the endpoint's to describe.
func cancelled(req *http.Request, err error) (problem.Problem, bool) {
	cause := req.Context().Err()
	if cause == nil || !errors.Is(err, cause) {
		return problem.Problem{}, false
	}
	if cause == context.Canceled {
		return problem.Problem{Status: statusClientClosedRequest}, true
	}
	return problem.Problem{Status: http.StatusServiceUnavailable}, true
}

func internalProblem() problem.Problem {
	return problem.Problem{Status: http.StatusInternalServerError}
}

// fail answers err as a problem. The response must not have started.
func (rs *resolver) fail(w http.ResponseWriter, req *http.Request, err error) {
	p, report := rs.resolve(req, err)
	rs.observe(req, report, p)
	rs.write(w, req, p)
}

func (rs *resolver) observe(req *http.Request, err error, p problem.Problem) {
	if rs.onError != nil {
		rs.onError(Failure{Request: req, Err: err, Problem: clone(p)})
	}
}

func (rs *resolver) write(w http.ResponseWriter, req *http.Request, p problem.Problem) {
	h := w.Header()
	h.Del("Content-Length")
	if tw, ok := w.(*trackingWriter); ok {
		for i, k := range representationFields {
			if tw.inherited&(1<<i) == 0 {
				h.Del(k)
			}
		}
	}
	if p.Status == statusClientClosedRequest {
		w.WriteHeader(p.Status)
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
		_ = f.Write(&buf, p)
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
	w.WriteHeader(p.Status)
	_, _ = w.Write(buf.Bytes())
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
