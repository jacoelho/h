package problem

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"slices"
)

// Format writes a Problem's body as one media type. The status and header
// fields are written by the caller; a Format only writes the body.
type Format interface {
	// MediaType returns the concrete media type the format writes.
	MediaType() string
	// Write writes p's body to w.
	Write(w io.Writer, p Problem) error
}

// RFC9457 writes problem details as application/problem+json (RFC 9457 §3):
// type, title, status, detail and the extension members.
var RFC9457 Format = rfc9457{}

// Text writes the problem's title as text/plain. The h router also writes
// it when the negotiated format fails, so it never fails for a valid
// Problem.
var Text Format = text{}

type rfc9457 struct{}

func (rfc9457) MediaType() string { return "application/problem+json" }

func (rfc9457) Write(w io.Writer, p Problem) error {
	enc := jsontext.NewEncoder(w)
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	member := func(name string, v any) error {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		return json.MarshalEncode(enc, v)
	}
	members := []struct {
		name string
		v    any
		omit bool
	}{
		{memberType, p.TypeOrBlank(), false},
		{memberTitle, p.TitleOrStatus(), p.TitleOrStatus() == ""},
		{memberStatus, p.Status, false},
		{memberDetail, p.Detail, p.Detail == ""},
	}
	for _, m := range members {
		if m.omit {
			continue
		}
		if err := member(m.name, m.v); err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Extra)) {
		if err := member(k, p.Extra[k]); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

type text struct{}

func (text) MediaType() string { return "text/plain; charset=utf-8" }

func (text) Write(w io.Writer, p Problem) error {
	_, err := io.WriteString(w, p.TitleOrStatus()+"\n")
	return err
}

// JSON returns a Format writing shape(p) as JSON under mediaType, for an API
// whose clients read an error shape of their own.
func JSON(mediaType string, shape func(Problem) any) Format { //nolint:iface,ireturn // Config.ProblemFormats takes Formats; jsonShape stays unexported.
	return jsonShape{mediaType: mediaType, shape: shape}
}

type jsonShape struct {
	mediaType string
	shape     func(Problem) any
}

func (f jsonShape) MediaType() string { return f.mediaType }

func (f jsonShape) Write(w io.Writer, p Problem) error {
	if f.shape == nil {
		return errors.New("problem: JSON format without a shape function")
	}
	return json.MarshalWrite(w, f.shape(p))
}
