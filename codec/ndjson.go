package codec

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
)

// NDJSON encodes and decodes sequences as newline-delimited JSON, one value
// per line. It flushes after every line when the writer can, so a reader
// sees each element as it is produced. It accepts only sequence types.
type NDJSON struct {
	// Options are passed to every marshal and unmarshal call.
	Options []json.Options
}

// CanEncode reports whether t is written as a sequence.
func (NDJSON) CanEncode(t reflect.Type) bool { return IsSequence(t) }

// CanDecode reports whether t is read as a sequence.
func (NDJSON) CanDecode(t reflect.Type) bool { return IsSequenceTarget(t) }

// Encode writes each element of v, a Sequence, on its own line.
func (c NDJSON) Encode(w io.Writer, v any) error {
	seq, ok := v.(Sequence)
	if !ok {
		return fmt.Errorf("codec: NDJSON cannot encode %T", v)
	}
	flusher, _ := w.(interface{ Flush() error })
	for e, err := range seq.Elems() {
		if err != nil {
			return err
		}
		if err := json.MarshalWrite(w, e, c.Options...); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if flusher != nil {
			if err := flusher.Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Decode binds v, a SequenceTarget, to read one JSON value per element.
func (c NDJSON) Decode(r io.Reader, v any) error {
	t, ok := v.(SequenceTarget)
	if !ok {
		return fmt.Errorf("codec: NDJSON cannot decode into %T", v)
	}
	dec := jsontext.NewDecoder(r, c.Options...)
	t.Bind(func(into any) (bool, error) {
		if dec.PeekKind() == 0 {
			if _, err := dec.ReadToken(); err != nil && !errors.Is(err, io.EOF) {
				return false, malformed(err)
			}
			return false, nil
		}
		if err := json.UnmarshalDecode(dec, into, c.Options...); err != nil {
			return false, malformed(err)
		}
		return true, nil
	})
	return nil
}
