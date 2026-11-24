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

// identity returns the identity function from A to B, and whether there is
// one: whether A and B are the same type.
func identity[A, B any]() (func(A) B, bool) {
	f, ok := any(func(a A) A { return a }).(func(A) B)
	return f, ok
}

func registerEndpoint[In, Out any](r *Router, method, path string, e Endpoint[In, Out], opts []Option) {
	if !r.mutable(fmt.Sprintf("%s %q registered", method, path)) {
		return
	}
	if e == nil {
		r.core.failf("%s %q: nil endpoint", method, path)
		return
	}
	inT, outT := reflect.TypeFor[In](), reflect.TypeFor[Out]()
	if err := errors.Join(checkNotNamedEmpty(inT, "input"), checkNotNamedEmpty(outT, "output")); err != nil {
		r.core.failf("%s %q: %v", method, path, err)
		return
	}
	inK, outK := inKindOf(inT), outKindOf(outT)
	r.register(method, path, opts, func(res resolved, _ string) (http.Handler, error) {
		var decodeT, encodeT reflect.Type
		if inK == inDecoded {
			decodeT = inT
		}
		if outK == outEncoded {
			encodeT = outT
		}
		route, err := res.codecs.For(decodeT, encodeT, res.consumes, res.produces)
		if err != nil {
			return nil, err
		}
		if inK == inRaw && len(res.consumes) == 0 {
			return nil, errors.New("a RawBody route must set Consumes")
		}
		a := &adapter[In, Out]{
			endpoint: e, base: r.core.base, res: res, route: route, in: inK, out: outK,
			duplex: inK == inRaw || codec.IsSequenceTarget(inT),
		}
		var ok bool
		switch inK {
		case inForm:
			a.fromForm, ok = identity[Form, In]()
		case inRaw:
			a.fromRaw, ok = identity[RawBody, In]()
		default:
			ok = true
		}
		if outK == outStream && ok {
			a.toStream, ok = identity[Out, Stream]()
		}
		if !ok {
			return nil, fmt.Errorf("input %v or output %v does not match its kind", inT, outT)
		}
		return a, nil
	})
}

type adapter[In, Out any] struct {
	endpoint Endpoint[In, Out]
	// fromForm, fromRaw and toStream are set for the kinds of In and Out
	// that are Form, RawBody and Stream, so a request needs no assertion.
	fromForm func(Form) In
	fromRaw  func(RawBody) In
	toStream func(Out) Stream
	base     string
	route    codec.Route
	res      resolved
	in       inKind
	out      outKind
	duplex   bool // the endpoint reads the body while the response is written
}

// exchange is the state of one request through an endpoint, allocated once
// per request: the writer, the request's parsed values, the body reader and
// the response writers all live in it.
type exchange struct {
	body  bodyReader
	st    requestState
	spill spillWriter
	// cleanup removes multipart temporary files. It is set as soon as the
	// form is parsed, so it runs even when the endpoint panics.
	cleanup     func() error
	tw          trackingWriter
	stream      StreamWriter
	contentType string
	header      []headerField
	status      int
}

// runStream runs the stream's write function with a writer that closes
// when the function returns or panics, so a writer it handed to a goroutine
// cannot reach a finished response.
func (x *exchange) runStream(s Stream) error {
	x.stream = StreamWriter{w: &x.tw, x: x}
	defer func() { x.stream = StreamWriter{} }()
	return s.write(&x.stream)
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
	in In
	exchange
}

func (a *adapter[In, Out]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rs := a.res.resolver
	c := &call[In]{}
	x := &c.exchange
	x.tw.ResponseWriter = w
	x.st = requestState{req: req, base: a.base}
	x.tw.inherit()
	x.body.init(&x.tw, req, a.res.maxBody)
	req.Body = &x.body
	defer func() {
		if x.cleanup == nil {
			return
		}
		if err := x.cleanup(); err != nil {
			rs.observe(req, fmt.Errorf("%w: %w", errMultipartCleanup, err), problem.Problem{})
		}
	}()
	if err := contain(func() error { return a.serve(c, req) }); err != nil {
		x.tw.answer(req, rs, err)
	}
}

// serve runs the request through the endpoint and writes the response,
// returning an error for the caller to answer. An encoded response
// negotiates its encoder before the endpoint runs, so a request it cannot
// answer is refused without calling it.
func (a *adapter[In, Out]) serve(c *call[In], req *http.Request) error {
	if a.out != outEncoded {
		resp, err := a.respond(c, req)
		if err != nil {
			return err
		}
		return a.writeUnencoded(&c.exchange, resp)
	}
	enc, contentType, err := a.route.Encoder(req.Header.Values("Accept"))
	if err != nil {
		return &RequestError{Status: http.StatusNotAcceptable, Err: err}
	}
	resp, err := a.respond(c, req)
	if err != nil {
		return err
	}
	return a.writeEncoded(&c.exchange, resp, enc, contentType)
}

// respond reads the input, calls the endpoint and keeps the status and
// header fields of a valid response.
func (a *adapter[In, Out]) respond(c *call[In], req *http.Request) (Response[Out], error) {
	if err := a.readInput(c, req); err != nil {
		return Response[Out]{}, err
	}
	resp, err := a.endpoint(req.Context(), Request[In]{st: &c.st, body: c.in})
	if err != nil {
		return Response[Out]{}, err
	}
	if err := resp.validate(a.out == outEmpty); err != nil {
		return Response[Out]{}, err
	}
	c.status, c.header = resp.Status(), resp.header
	return resp, nil
}

func (a *adapter[In, Out]) readInput(c *call[In], req *http.Request) error {
	if a.in == inEmpty {
		return nil
	}
	x := &c.exchange
	if a.duplex {
		// Without full duplex, an HTTP/1 server consumes the unread request
		// body before writing the response (net/http documentation of
		// ResponseController.EnableFullDuplex). Writers that cannot enable
		// it report ErrNotSupported, which needs no handling: HTTP/2 is
		// full duplex already.
		_ = http.NewResponseController(&x.tw).EnableFullDuplex() //nolint:errcheck // ErrNotSupported is the only failure; see above.
	}
	ct := req.Header.Get("Content-Type")
	match := ct
	if match == "" {
		match = "application/octet-stream" // RFC 9110, section 8.3
	}
	switch a.in {
	case inForm:
		if !a.route.Consumes(match) {
			return &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
		}
		f, cleanup, err := parseForm(&x.st, &x.body, a.res.maxMemory)
		x.cleanup = cleanup
		if err != nil {
			return err
		}
		c.in = a.fromForm(f)
		return nil
	case inRaw:
		if !a.route.Consumes(match) {
			return &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
		}
		c.in = a.fromRaw(RawBody{contentType: ct, r: &x.body})
		return nil
	default:
		dec, err := a.route.Decoder(match)
		if err != nil {
			return &RequestError{Status: http.StatusUnsupportedMediaType, Err: err}
		}
		if err := dec.Decode(&x.body, &c.in); err != nil {
			return decodeFault(err)
		}
		return nil
	}
}

// writeUnencoded writes a response whose body no codec writes: none, or a
// Stream.
func (a *adapter[In, Out]) writeUnencoded(x *exchange, resp Response[Out]) error {
	if a.out == outEmpty || resp.bodiless {
		x.start()
		return nil
	}
	s := a.toStream(resp.Body())
	if s.write == nil {
		return fmt.Errorf("%w: zero Stream", ErrInvalidResponse)
	}
	x.contentType = s.contentType
	err := x.runStream(s)
	if err != nil {
		return err
	}
	if !x.tw.wrote {
		x.start()
	}
	return nil
}

// writeEncoded writes a response whose body enc writes as contentType.
func (a *adapter[In, Out]) writeEncoded(x *exchange, resp Response[Out], enc codec.Encoder, contentType string) error {
	if a.route.Choices() > 1 {
		addVary(x.tw.Header(), "Accept")
	}
	if resp.bodiless {
		x.start()
		return nil
	}
	x.contentType = contentType
	x.spill = spillWriter{x: x, limit: a.res.spillAt}
	defer x.spill.release()
	body := any(resp.Body())
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
