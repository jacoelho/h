package codec

import (
	"encoding"
	"fmt"
	"io"
	"reflect"
)

// Text encodes and decodes strings, byte slices and values implementing
// encoding.TextMarshaler and encoding.TextUnmarshaler, as plain text.
type Text struct{}

var (
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// CanEncode reports whether t is a string, a byte slice, or implements
// encoding.TextMarshaler.
func (Text) CanEncode(t reflect.Type) bool { return isText(t) || t.Implements(textMarshalerType) }

// CanDecode reports whether t is a string, a byte slice, or its pointer
// implements encoding.TextUnmarshaler.
func (Text) CanDecode(t reflect.Type) bool {
	return isText(t) || reflect.PointerTo(t).Implements(textUnmarshalerType)
}

func isText(t reflect.Type) bool {
	return t.Kind() == reflect.String || (t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8)
}

// Encode writes v as text.
func (Text) Encode(w io.Writer, v any) error {
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	case []byte:
		b = x
	case encoding.TextMarshaler:
		var err error
		if b, err = x.MarshalText(); err != nil {
			return err
		}
	default:
		rv := reflect.ValueOf(v)
		switch {
		case rv.Kind() == reflect.String:
			b = []byte(rv.String())
		case rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8:
			b = rv.Bytes()
		default:
			return fmt.Errorf("codec: Text cannot encode %T", v)
		}
	}
	_, err := w.Write(b)
	return err
}

// Decode reads the whole body as text into v.
func (Text) Decode(r io.Reader, v any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	switch x := v.(type) {
	case *string:
		*x = string(b)
	case *[]byte:
		*x = b
	case encoding.TextUnmarshaler:
		if err := x.UnmarshalText(b); err != nil {
			return &DecodeError{Err: err}
		}
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer {
			return fmt.Errorf("codec: Text cannot decode into %T", v)
		}
		e := rv.Elem()
		switch {
		case e.Kind() == reflect.String:
			e.SetString(string(b))
		case e.Kind() == reflect.Slice && e.Type().Elem().Kind() == reflect.Uint8:
			e.SetBytes(b)
		default:
			return fmt.Errorf("codec: Text cannot decode into %T", v)
		}
	}
	return nil
}
