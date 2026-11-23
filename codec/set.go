package codec

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/jacoelho/h/internal/media"
)

// Set is an ordered registration of decoders and encoders by media type.
// Order breaks ties in negotiation. A Set must not change once a router uses
// it; reading it concurrently is safe.
type Set struct {
	decoders []decoding
	encoders []encodingReg
}

type decoding struct {
	dec    Decoder
	ranges []media.Type
}

type encodingReg struct {
	enc  Encoder
	name string
	typ  media.Type
}

// NewSet returns an empty Set.
func NewSet() *Set { return &Set{} }

// Default returns a Set decoding and encoding JSON (application/json, and
// application/*+json for decoding) and text (text/plain).
func Default() *Set {
	return &Set{
		decoders: []decoding{
			{dec: JSON{}, ranges: []media.Type{{Main: "application", Sub: "json"}, {Main: "application", Sub: "*+json"}}},
			{dec: Text{}, ranges: []media.Type{{Main: "text", Sub: "plain"}}},
		},
		encoders: []encodingReg{
			{enc: JSON{}, name: "application/json", typ: media.Type{Main: "application", Sub: "json"}},
			{enc: Text{}, name: "text/plain; charset=utf-8", typ: media.Type{Main: "text", Sub: "plain"}},
		},
	}
}

// Decodes registers d for request bodies whose media type matches one of
// mediaRanges, such as "application/json" or "application/*+json". It
// returns an error, registering nothing, for a nil decoder or a malformed
// range.
func (s *Set) Decodes(d Decoder, mediaRanges ...string) error {
	if d == nil {
		return errors.New("codec: nil decoder")
	}
	reg := decoding{dec: d}
	for _, r := range mediaRanges {
		t, ok := media.Parse(r)
		if !ok {
			return fmt.Errorf("codec: malformed media range %q", r)
		}
		reg.ranges = append(reg.ranges, t)
	}
	s.decoders = append(s.decoders, reg)
	return nil
}

// Encodes registers e for each of mediaTypes, which name concrete types. A
// chosen media type, with its parameters as written here, becomes the
// response's Content-Type. It returns an error, registering nothing, for a
// nil encoder or a malformed or wildcard type.
func (s *Set) Encodes(e Encoder, mediaTypes ...string) error {
	if e == nil {
		return errors.New("codec: nil encoder")
	}
	var regs []encodingReg
	for _, name := range mediaTypes {
		t, ok := media.Parse(name)
		if !ok || !t.Concrete() {
			return fmt.Errorf("codec: Encodes needs concrete media types, not %q", name)
		}
		regs = append(regs, encodingReg{enc: e, name: name, typ: t})
	}
	s.encoders = append(s.encoders, regs...)
	return nil
}

// ErrUnsupportedMediaType reports a request body in a media type the route
// does not decode.
var ErrUnsupportedMediaType = errors.New("codec: unsupported media type")

// ErrNotAcceptable reports a request whose Accept field admits none of the
// media types the route encodes.
var ErrNotAcceptable = errors.New("codec: not acceptable")

// Route is the part of a Set one route can use: the decoders that read its
// input type and the encoders that write its output type, narrowed to the
// media ranges the route consumes and produces.
type Route struct {
	consumes []media.Type
	decoders []decoding
	encoders []encodingReg
	offers   []media.Type
}

// For returns the codecs of s that can read in and write out, keeping only
// media types matched by consumes and produces when those are non-empty.
// A nil type skips that direction.
func (s *Set) For(in, out reflect.Type, consumes, produces []string) (Route, error) {
	cons, err := parseAll(consumes)
	if err != nil {
		return Route{}, err
	}
	r := Route{consumes: cons}
	prods, err := parseAll(produces)
	if err != nil {
		return Route{}, err
	}
	if in != nil {
		for _, d := range s.decoders {
			if !d.dec.CanDecode(in) {
				continue
			}
			ranges := d.ranges
			if len(cons) > 0 {
				ranges = narrow(ranges, cons)
			}
			if len(ranges) > 0 {
				r.decoders = append(r.decoders, decoding{dec: d.dec, ranges: ranges})
			}
		}
		if len(r.decoders) == 0 {
			return Route{}, fmt.Errorf("codec: no registered decoder reads %v", in)
		}
	}
	if out != nil {
		for _, e := range s.encoders {
			if !e.enc.CanEncode(out) {
				continue
			}
			if len(prods) > 0 && !slices.ContainsFunc(prods, func(p media.Type) bool { return p.Matches(e.typ) }) {
				continue
			}
			r.encoders = append(r.encoders, e)
			r.offers = append(r.offers, e.typ)
		}
		if len(r.encoders) == 0 {
			return Route{}, fmt.Errorf("codec: no registered encoder writes %v", out)
		}
	}
	return r, nil
}

// narrow returns, for each pair of a registered range and a consumed range
// where one includes the other, the narrower of the two.
func narrow(registered, consumed []media.Type) []media.Type {
	var out []media.Type
	for _, r := range registered {
		for _, c := range consumed {
			switch {
			case r.Matches(c):
				out = append(out, c)
			case c.Matches(r):
				out = append(out, r)
			}
		}
	}
	return out
}

func parseAll(names []string) ([]media.Type, error) {
	var out []media.Type
	for _, n := range names {
		t, ok := media.Parse(n)
		if !ok {
			return nil, fmt.Errorf("codec: malformed media range %q", n)
		}
		out = append(out, t)
	}
	return out, nil
}

// Decoder returns the decoder for a request's Content-Type, or
// ErrUnsupportedMediaType.
func (r Route) Decoder(contentType string) (Decoder, error) {
	t, ok := concrete(contentType)
	if !ok {
		return nil, ErrUnsupportedMediaType
	}
	for _, d := range r.decoders {
		for _, rg := range d.ranges {
			if rg.Matches(t) {
				return d.dec, nil
			}
		}
	}
	return nil, ErrUnsupportedMediaType
}

// Consumes reports whether a body of contentType, a concrete media type,
// is within the ranges the route consumes; a route consuming nothing in
// particular takes any.
func (r Route) Consumes(contentType string) bool {
	t, ok := concrete(contentType)
	return ok && (len(r.consumes) == 0 || slices.ContainsFunc(r.consumes, func(c media.Type) bool { return c.Matches(t) }))
}

func concrete(contentType string) (media.Type, bool) {
	t, ok := media.Parse(contentType)
	return t, ok && t.Concrete()
}

// Encoder returns the encoder for a request's Accept field values and the
// Content-Type to send, or ErrNotAcceptable.
func (r Route) Encoder(accept []string) (Encoder, string, error) {
	i, ok := media.Negotiate(accept, r.offers, false)
	if !ok {
		return nil, "", ErrNotAcceptable
	}
	return r.encoders[i].enc, r.encoders[i].name, nil
}

// Choices reports how many encoders the route negotiates between.
func (r Route) Choices() int { return len(r.encoders) }
