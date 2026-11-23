package codec_test

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/jacoelho/h/codec"
)

type order struct {
	ID    string `json:"id"`
	Total int    `json:"total"`
}

func TestJSONDecodeRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		c    codec.JSON
	}{
		{"empty body", "", codec.JSON{}},
		{"trailing data", `{"id":"a"} {"id":"b"}`, codec.JSON{}},
		{"duplicate member", `{"id":"a","id":"b"}`, codec.JSON{}},
		{"wrong type", `{"total":"ten"}`, codec.JSON{}},
		{"unknown member when rejected", `{"id":"a","extra":1}`, codec.JSON{RejectUnknownFields: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var o order
			err := c.c.Decode(strings.NewReader(c.body), &o)
			if _, ok := errors.AsType[*codec.DecodeError](err); !ok {
				t.Fatalf("Decode(%q) = %v, want *DecodeError", c.body, err)
			}
		})
	}
}

func TestJSONDecodeAcceptsUnknownMembersByDefault(t *testing.T) {
	var o order
	if err := (codec.JSON{}).Decode(strings.NewReader(`{"id":"a","extra":1}`), &o); err != nil || o.ID != "a" {
		t.Fatalf("got %+v, %v", o, err)
	}
}

func TestJSONDecodeKeepsReaderErrors(t *testing.T) {
	readErr := errors.New("connection reset")
	var o order
	err := (codec.JSON{}).Decode(io.MultiReader(strings.NewReader(`{"id":`), &failingReader{readErr}), &o)
	if !errors.Is(err, readErr) {
		t.Fatalf("Decode = %v, want it to wrap the reader's error", err)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestJSONOptionsReplaceReflection(t *testing.T) {
	c := codec.JSON{Options: []json.Options{json.WithMarshalers(json.MarshalToFunc(func(e *jsontext.Encoder, o order) error {
		return e.WriteToken(jsontext.String("order:" + o.ID))
	}))}}
	var buf bytes.Buffer
	if err := c.Encode(&buf, order{ID: "7"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != `"order:7"` {
		t.Fatalf("got %s", got)
	}
}

type elems []any

func (e elems) Elems() iter.Seq2[any, error] {
	return func(yield func(any, error) bool) {
		for _, v := range e {
			if !yield(v, nil) {
				return
			}
		}
	}
}

func TestSequencesEncode(t *testing.T) {
	seq := elems{order{"a", 1}, order{"b", 2}}
	cases := []struct {
		name string
		enc  codec.Encoder
		want string
	}{
		{"JSON array", codec.JSON{}, `[{"id":"a","total":1},{"id":"b","total":2}]`},
		{"NDJSON lines", codec.NDJSON{}, "{\"id\":\"a\",\"total\":1}\n{\"id\":\"b\",\"total\":2}\n"},
		{"empty JSON array", codec.JSON{}, `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			v := seq
			if strings.HasPrefix(c.name, "empty") {
				v = nil
			}
			if err := c.enc.Encode(&buf, v); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			if _, ok := c.enc.(codec.JSON); ok {
				got = strings.TrimSpace(got)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

type target struct {
	next func(any) (bool, error)
}

func (t *target) Bind(next func(any) (bool, error)) { t.next = next }

func readAll(t *testing.T, dec codec.Decoder, body string) ([]order, error) {
	t.Helper()
	var tg target
	if err := dec.Decode(strings.NewReader(body), &tg); err != nil {
		return nil, err
	}
	var out []order
	for {
		var o order
		more, err := tg.next(&o)
		if err != nil || !more {
			return out, err
		}
		out = append(out, o)
	}
}

func TestSequencesDecode(t *testing.T) {
	cases := []struct {
		name    string
		dec     codec.Decoder
		body    string
		want    int
		wantErr bool
	}{
		{"JSON array", codec.JSON{}, `[{"id":"a"},{"id":"b"}]`, 2, false},
		{"empty JSON array", codec.JSON{}, `[]`, 0, false},
		{"malformed third element", codec.JSON{}, `[{"id":"a"},{"id":"b"},{"id":3}]`, 2, true},
		{"not an array", codec.JSON{}, `{"id":"a"}`, 0, true},
		{"trailing data after the array", codec.JSON{}, `[{"id":"a"}] x`, 1, true},
		{"NDJSON", codec.NDJSON{}, "{\"id\":\"a\"}\n{\"id\":\"b\"}\n", 2, false},
		{"NDJSON malformed line", codec.NDJSON{}, "{\"id\":\"a\"}\n{\"id\":\n", 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readAll(t, c.dec, c.body)
			if len(got) != c.want {
				t.Fatalf("read %d elements, want %d (err %v)", len(got), c.want, err)
			}
			if c.wantErr {
				if _, ok := errors.AsType[*codec.DecodeError](err); !ok {
					t.Fatalf("err = %v, want *DecodeError", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetForNarrowsByTypeAndRanges(t *testing.T) {
	s := codec.Default(codec.JSON{})
	if err := s.Encodes(codec.NDJSON{}, "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	structT := reflect.TypeFor[order]()
	r, err := s.For(structT, structT, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Choices() != 1 {
		t.Fatalf("struct output has %d encoders, want 1 (JSON only)", r.Choices())
	}
	if _, _, err := r.Encoder([]string{"text/plain"}); !errors.Is(err, codec.ErrNotAcceptable) {
		t.Fatalf("text/plain for a struct: %v, want ErrNotAcceptable", err)
	}
	if _, err := r.Decoder("application/vnd.api+json"); err != nil {
		t.Fatalf("+json body: %v", err)
	}
	if _, err := r.Decoder("text/plain"); !errors.Is(err, codec.ErrUnsupportedMediaType) {
		t.Fatalf("text body into a struct: %v", err)
	}

	strT := reflect.TypeFor[string]()
	r, err = s.For(nil, strT, nil, []string{"text/*"})
	if err != nil {
		t.Fatal(err)
	}
	_, ct, err := r.Encoder(nil)
	if err != nil || ct != "text/plain; charset=utf-8" {
		t.Fatalf("Produces(text/*) chose %q, %v", ct, err)
	}

	if _, err := s.For(nil, reflect.TypeFor[func()](), nil, nil); err == nil {
		t.Fatal("no encoder for a func type, want an error")
	}
}

func TestEncodesRefusesWildcard(t *testing.T) {
	s := codec.NewSet()
	if err := s.Encodes(codec.JSON{}, "application/json", "application/*"); err == nil {
		t.Fatal("wildcard accepted")
	}
	if _, err := s.For(nil, reflect.TypeFor[order](), nil, nil); err == nil {
		t.Fatal("a refused registration left encoders behind")
	}
}

func TestTextRoundTrip(t *testing.T) {
	type label string
	var buf bytes.Buffer
	if err := (codec.Text{}).Encode(&buf, label("hello")); err != nil {
		t.Fatal(err)
	}
	var got label
	if err := (codec.Text{}).Decode(&buf, &got); err != nil || got != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
}

type marshalOnly struct{}

func (marshalOnly) MarshalText() ([]byte, error) { return []byte("m"), nil }

type unmarshalOnly struct{}

func (*unmarshalOnly) UnmarshalText([]byte) error { return nil }

type bothText struct{}

func (bothText) MarshalText() ([]byte, error) { return []byte("b"), nil }
func (*bothText) UnmarshalText([]byte) error  { return nil }

func TestTextChecksEachDirection(t *testing.T) {
	cases := []struct {
		typ                  reflect.Type
		canEncode, canDecode bool
	}{
		{reflect.TypeFor[marshalOnly](), true, false},
		{reflect.TypeFor[unmarshalOnly](), false, true},
		{reflect.TypeFor[bothText](), true, true},
		{reflect.TypeFor[string](), true, true},
		{reflect.TypeFor[int](), false, false},
	}
	for _, c := range cases {
		if got := (codec.Text{}).CanEncode(c.typ); got != c.canEncode {
			t.Errorf("CanEncode(%v) = %v, want %v", c.typ, got, c.canEncode)
		}
		if got := (codec.Text{}).CanDecode(c.typ); got != c.canDecode {
			t.Errorf("CanDecode(%v) = %v, want %v", c.typ, got, c.canDecode)
		}
	}
}

func TestJSONOptionsApplyToSequenceElements(t *testing.T) {
	_, err := readAll(t, codec.JSON{RejectUnknownFields: true}, `[{"id":"a","extra":1}]`)
	if _, ok := errors.AsType[*codec.DecodeError](err); !ok {
		t.Fatalf("unknown member in an element: %v, want *DecodeError", err)
	}
	c := codec.JSON{Options: []json.Options{json.WithMarshalers(json.MarshalToFunc(func(e *jsontext.Encoder, o order) error {
		return e.WriteToken(jsontext.String("order:" + o.ID))
	}))}}
	var buf bytes.Buffer
	if err := c.Encode(&buf, elems{order{ID: "7"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != `["order:7"]` {
		t.Fatalf("got %s", got)
	}
}

func TestJSONSequenceKeepsReadErrorAfterClosingBracket(t *testing.T) {
	readErr := errors.New("connection reset")
	var tg target
	if err := (codec.JSON{}).Decode(io.MultiReader(strings.NewReader(`[{"id":"a"}]`), &failingReader{readErr}), &tg); err != nil {
		t.Fatal(err)
	}
	var err error
	for more := true; more && err == nil; {
		var o order
		more, err = tg.next(&o)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
}
