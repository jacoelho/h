// Package codec encodes response bodies and decodes request bodies, and holds
// the set of codecs a router negotiates between.
//
// A codec is chosen per request from the media types registered in a Set:
// decoders by the request's Content-Type, encoders by its Accept field
// (RFC 9110 §12.5.1). Codecs never write header fields.
package codec

import (
	"io"
	"iter"
	"reflect"
)

// Encoder writes values as one or more media types.
type Encoder interface {
	// Encode writes v to w.
	Encode(w io.Writer, v any) error
	// CanEncode reports whether Encode can write values of type t. It is
	// called once per route, when the route is registered.
	CanEncode(t reflect.Type) bool
}

// Decoder reads values from one or more media types.
type Decoder interface {
	// Decode reads a value from r into v, a non-nil pointer. Malformed input
	// is reported as a *DecodeError.
	Decode(r io.Reader, v any) error
	// CanDecode reports whether Decode can read into values of type t. It
	// is called once per route, when the route is registered.
	CanDecode(t reflect.Type) bool
}

// DecodeError reports input a decoder could not read as the value asked for.
type DecodeError struct {
	Err error
}

// Error describes the malformed input.
func (e *DecodeError) Error() string { return "codec: malformed input: " + e.Err.Error() }

// Unwrap returns the decoder's error.
func (e *DecodeError) Unwrap() error { return e.Err }

// Sequence is a value written as a sequence of elements, such as a JSON
// array or newline-delimited JSON, without holding them all in memory.
// Encoders that support sequences range over Elems and stop at the first
// error, returning it.
type Sequence interface {
	Elems() iter.Seq2[any, error]
}

// SequenceTarget is a value read as a sequence of elements while the
// consumer ranges over it. A decoder that supports sequences calls Bind with
// a function that reads the next element into the pointer it is given and
// reports whether there was one.
type SequenceTarget interface {
	Bind(next func(into any) (more bool, err error))
}

var (
	sequenceType       = reflect.TypeFor[Sequence]()
	sequenceTargetType = reflect.TypeFor[SequenceTarget]()
)

// IsSequence reports whether values of type t are written as sequences.
func IsSequence(t reflect.Type) bool { return t.Implements(sequenceType) }

// IsSequenceTarget reports whether values of type t are read as sequences.
func IsSequenceTarget(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(sequenceTargetType)
}
