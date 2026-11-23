package codec

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"reflect"
)

// JSON encodes and decodes JSON with encoding/json/v2. Decoding rejects
// duplicate member names and trailing data. Values implementing Sequence are
// written as a JSON array element by element; values implementing
// SequenceTarget read a JSON array element by element.
type JSON struct {
	// RejectUnknownFields refuses object members that match no field.
	RejectUnknownFields bool
	// Options are passed to every marshal and unmarshal call, for example
	// json.WithMarshalers to replace reflection for hot types.
	Options []json.Options
}

var rejectUnknown = []json.Options{json.RejectUnknownMembers(true)}

// options returns the options of one Encode or Decode call. Unknown members
// are allowed by default in json/v2, so the common case passes none.
func (c JSON) options() []json.Options {
	if !c.RejectUnknownFields {
		return c.Options
	}
	if len(c.Options) == 0 {
		return rejectUnknown
	}
	return append(rejectUnknown[:1:1], c.Options...)
}

// CanEncode reports whether t is a type JSON can represent.
func (c JSON) CanEncode(t reflect.Type) bool { return c.represents(t) }

// CanDecode reports whether t is a type JSON can represent.
func (c JSON) CanDecode(t reflect.Type) bool { return c.represents(t) }

func (JSON) represents(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Func, reflect.Chan, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		return IsSequence(t) || IsSequenceTarget(t)
	}
	return true
}

// Encode writes v as JSON.
func (c JSON) Encode(w io.Writer, v any) error {
	if seq, ok := v.(Sequence); ok {
		return c.encodeSeq(w, seq)
	}
	return json.MarshalWrite(w, v, c.options()...)
}

// encodeSeq is apart from Encode because ranging over a function moves
// the caller's results to the heap, which would cost every non-sequence
// Encode an allocation.
func (c JSON) encodeSeq(w io.Writer, seq Sequence) error {
	enc := jsontext.NewEncoder(w, c.options()...)
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}
	for e, err := range seq.Elems() {
		if err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, e); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndArray)
}

// Decode reads one JSON value into v, or binds v to read a JSON array lazily
// when it implements SequenceTarget.
func (c JSON) Decode(r io.Reader, v any) error {
	if t, ok := v.(SequenceTarget); ok {
		dec := jsontext.NewDecoder(r, c.options()...)
		started := false
		t.Bind(func(into any) (bool, error) {
			if !started {
				tok, err := dec.ReadToken()
				if err != nil {
					return false, malformed(err)
				}
				if tok.Kind() != '[' {
					return false, &DecodeError{Err: errors.New("want a JSON array")}
				}
				started = true
			}
			if dec.PeekKind() == ']' {
				if _, err := dec.ReadToken(); err != nil {
					return false, malformed(err)
				}
				switch _, err := dec.ReadToken(); {
				case err == nil:
					return false, &DecodeError{Err: errors.New("trailing data after the JSON array")}
				case !errors.Is(err, io.EOF):
					return false, malformed(err)
				}
				return false, nil
			}
			if err := json.UnmarshalDecode(dec, into); err != nil {
				return false, malformed(err)
			}
			return true, nil
		})
		return nil
	}
	if err := json.UnmarshalRead(r, v, c.options()...); err != nil {
		return malformed(err)
	}
	return nil
}

func malformed(err error) error {
	if _, ok := errors.AsType[*DecodeError](err); ok {
		return err
	}
	return &DecodeError{Err: err}
}
