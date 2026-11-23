package h

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/problem"
)

type inKind int

const (
	inEmpty inKind = iota
	inDecoded
	inForm
	inRaw
)

type outKind int

const (
	outEmpty outKind = iota
	outEncoded
	outStream
)

var (
	formType    = reflect.TypeFor[Form]()
	rawBodyType = reflect.TypeFor[RawBody]()
	streamType  = reflect.TypeFor[Stream]()
)

func checkNotNamedEmpty(t reflect.Type, role string) error {
	if t.Kind() == reflect.Struct && t.NumField() == 0 && t.Name() != "" {
		return fmt.Errorf("%s type %v has no fields; use h.Empty", role, t)
	}
	return nil
}

func inKindOf(t reflect.Type) inKind {
	switch t {
	case emptyType:
		return inEmpty
	case formType:
		return inForm
	case rawBodyType:
		return inRaw
	}
	return inDecoded
}

func outKindOf(t reflect.Type) outKind {
	switch t {
	case emptyType:
		return outEmpty
	case streamType:
		return outStream
	}
	return outEncoded
}

func registerEndpoint[In, Out any](r *Router, method, path string, e Endpoint[In, Out], opts []Option) {
	if !r.mutable(fmt.Sprintf("%s %q registered", method, path)) {
		return
	}
	if e == nil {
		r.core.fail("%s %q: nil endpoint", method, path)
		return
	}
	inT, outT := reflect.TypeFor[In](), reflect.TypeFor[Out]()
	if err := errors.Join(checkNotNamedEmpty(inT, "input"), checkNotNamedEmpty(outT, "output")); err != nil {
		r.core.fail("%s %q: %v", method, path, err)
		return
	}
	inK, outK := inKindOf(inT), outKindOf(outT)
	r.register(method, path, opts, func(res resolved, pattern string) (http.Handler, error) {
		var decodeT, encodeT reflect.Type
		if inK == inDecoded {
			decodeT = inT
		}
		if outK == outEncoded {
			encodeT = outT
		}
		route, err := r.core.codecs.For(decodeT, encodeT, res.consumes, res.produces)
		if err != nil {
			return nil, err
		}
		if inK == inRaw && len(res.consumes) == 0 {
			return nil, errors.New("a RawBody route must set Consumes")
		}
		return &adapter[In, Out]{endpoint: e, base: r.core.base, res: res, route: route, in: inK, out: outK}, nil
	})
}

type adapter[In, Out any] struct {
	endpoint Endpoint[In, Out]
	base     string
	res      resolved
	route    codec.Route
	in       inKind
	out      outKind
}

// exchange is the state of one request through an endpoint, allocated once
// per request: the writer, the request's parsed values, the body reader and
// the response writers all live in it.
type exchange struct {
	tw          trackingWriter
	st          requestState
	body        bodyReader
	spill       spillWriter
	stream      StreamWriter
	status      int
	contentType string
	header      []headerField
}

// start sends the status and header fields of a successful response.
func (x *exchange) start() {
	h := x.tw.Header()
	applyHeader(h, x.header)
	if x.contentType != "" {
		h.Set("Content-Type", x.contentType)
	}
	x.tw.WriteHeader(x.status)
}

type call[In any] struct {
	exchange
	in In
}

func (a *adapter[In, Out]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rs := a.res.resolver
	c := &call[In]{}
	x := &c.exchange
	x.tw.ResponseWriter = w
	x.st = requestState{req: req, base: a.base}
	x.body.init(&x.tw, req, a.res.maxBody)
	req.Body = &x.body
	var cleanup func() error
	defer func() {
		if cleanup == nil {
			return
		}
		if err := cleanup(); err != nil {
			rs.observe(req, fmt.Errorf("h: removing multipart files: %w", err), problem.Problem{})
		}
	}()
	err := contain(func() error {
		var err error
		cleanup, err = a.serve(c, req)
		return err
	})
	if err != nil {
		x.tw.answer(req, rs, err)
	}
}

// serve runs the request through the endpoint and writes the response,
// returning an error for the caller to answer.
func (a *adapter[In, Out]) serve(c *call[In], req *http.Request) (cleanup func() error, err error) {
	x := &c.exchange
	var enc codec.Encoder
	var contentType string
	if a.out == outEncoded {
		if enc, contentType, err = a.route.Encoder(req.Header.Values("Accept")); err != nil {
			return nil, &RequestError{Status: http.StatusNotAcceptable, Err: err}
		}
	}
	if cleanup, err = a.readInput(c, req); err != nil {
		return cleanup, err
	}
	resp, err := a.endpoint(req.Context(), Request[In]{st: &x.st, body: c.in})
	if err != nil {
		return cleanup, err
	}
	if err := resp.validate(a.out == outEmpty); err != nil {
		return cleanup, err
	}
	x.status, x.header = resp.Status(), resp.header
	return cleanup, a.write(x, resp, enc, contentType)
}

func (a *adapter[In, Out]) readInput(c *call[In], req *http.Request) (func() error, error) {
	if a.in == inEmpty {
		return nil, nil
	}
	x := &c.exchange
	ct := req.Header.Get("Content-Type")
	switch a.in {
	case inForm:
		if !a.route.Consumes(ct) {
			return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
		}
		f, cleanup, err := parseForm(&x.st, &x.body, a.res.maxMemory)
		if err != nil {
			return cleanup, err
		}
		c.in = any(f).(In)
		return cleanup, nil
	case inRaw:
		if !a.route.Consumes(ct) {
			return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
		}
		c.in = any(RawBody{contentType: ct, r: &x.body}).(In)
		return nil, nil
	}
	dec, err := a.route.Decoder(ct)
	if err != nil {
		return nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: err}
	}
	if err := dec.Decode(&x.body, &c.in); err != nil {
		return nil, decodeFault(err)
	}
	return nil, nil
}

func (a *adapter[In, Out]) write(x *exchange, resp Response[Out], enc codec.Encoder, contentType string) error {
	if a.route.Choices() > 1 {
		addVary(x.tw.Header(), "Accept")
	}
	if a.out == outEmpty || resp.bodiless {
		x.start()
		return nil
	}
	body := any(resp.Body())
	if a.out == outStream {
		s := body.(Stream)
		if s.write == nil {
			return fmt.Errorf("%w: zero Stream", ErrInvalidResponse)
		}
		x.contentType = s.contentType
		x.stream = StreamWriter{w: &x.tw, x: x}
		err := s.write(&x.stream)
		x.stream = StreamWriter{}
		if err != nil {
			return err
		}
		if !x.tw.wrote {
			x.start()
		}
		return nil
	}
	x.contentType = contentType
	x.spill = spillWriter{x: x, limit: a.res.spillAt}
	defer x.spill.release()
	err := enc.Encode(&x.spill, body)
	if so, ok := body.(seqOutput); ok {
		if serr := so.sourceErr(); serr != nil {
			err = serr
		}
	}
	if err == nil {
		return x.spill.finish()
	}
	if se, ok := errors.AsType[*sourceError](err); ok {
		err = se.err
	}
	return err
}
