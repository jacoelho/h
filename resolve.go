package h

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jacoelho/h/internal/media"
	"github.com/jacoelho/h/problem"
)

// OnError observes every error a route answers with: the error and the
// problem it became. An error found after the response was sent, such as
// failing to remove multipart temporary files, comes with the zero Problem. It runs before the problem is written and must not
// write to the response or panic: a panic is not recovered, since OnError is
// where it would be reported, and reaches the http.Server, which logs it
// and closes the connection.
type OnError func(ctx context.Context, r *http.Request, err error, p problem.Problem)

const statusClientClosedRequest = 499

// resolver turns errors into problems for one route or scope: the mappers
// in the order they are tried, the formats to write problems in, and the
// observer.
type resolver struct {
	mappers []problem.Mapper
	formats []problem.Format
	offers  []media.Type
	onError OnError
}

func newResolver(mappers []problem.Mapper, formats []problem.Format, onError OnError) (*resolver, error) {
	fs := append(slices.Clone(formats), problem.Text)
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
	return &resolver{mappers: mappers, formats: fs, offers: offers, onError: onError}, nil
}

// mapped asks the mappers in order. A mapper that panics answers nothing,
// and its panic is returned as a *PanicError for the caller to report.
func (rs *resolver) mapped(err error) (p problem.Problem, ok bool, perr error) {
	defer func() {
		if v := recover(); v != nil {
			p, ok, perr = problem.Problem{}, false, panicError(v)
		}
	}()
	for _, m := range rs.mappers {
		if p, ok := m.Problem(err); ok {
			return p, true, nil
		}
	}
	return problem.Problem{}, false, nil
}

// resolve returns the problem for err and the error to report, which adds
// ErrInvalidProblem when a mapper's answer was discarded.
func (rs *resolver) resolve(req *http.Request, err error) (problem.Problem, error) {
	if pe, ok := errors.AsType[*ParamsError](err); ok {
		return rs.present(err, pe.problem())
	}
	if re, ok := errors.AsType[*RequestError](err); ok {
		if re.Status < 400 || re.Status > 599 {
			return internalProblem(), errors.Join(err, fmt.Errorf("%w: RequestError status %d", ErrInvalidProblem, re.Status))
		}
		return rs.present(err, problem.Problem{Status: re.Status})
	}
	p, ok, perr := rs.mapped(err)
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

// present lets mappers describe an error whose status the router fixes.
func (rs *resolver) present(err error, fixed problem.Problem) (problem.Problem, error) {
	p, ok, perr := rs.mapped(err)
	if perr != nil {
		return fixed, errors.Join(err, perr)
	}
	if !ok {
		return fixed, err
	}
	if p.Status != fixed.Status {
		return fixed, errors.Join(err, fmt.Errorf("%w: status %d for an error the router answers with %d", ErrInvalidProblem, p.Status, fixed.Status))
	}
	if verr := p.Validate(); verr != nil {
		return fixed, errors.Join(err, fmt.Errorf("%w: %w", ErrInvalidProblem, verr))
	}
	return p, err
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
		rs.onError(req.Context(), req, err, p)
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
