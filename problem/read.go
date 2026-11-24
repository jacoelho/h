package problem

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
)

// Read decodes an RFC 9457 problem details JSON object: the standard
// members into their fields, every other member into Extra. It reports an
// error, wrapping ErrInvalid, when the input is not a JSON object or its
// status is present but null or not an integer. A missing status leaves
// Status zero: RFC 9457, section 3.1.2, makes the member advisory.
//
// Read returns what the body says, not what RFC9457 would write: it does
// not apply Validate, Extra holds JSON numbers as float64, and the
// "instance" member lands in Extra. Header is never set: header fields are
// not part of the body.
func Read(r io.Reader) (Problem, error) {
	var members map[string]jsontext.Value
	if err := json.UnmarshalRead(r, &members); err != nil {
		return Problem{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if members == nil {
		return Problem{}, fmt.Errorf("%w: not a JSON object", ErrInvalid)
	}
	var p Problem
	if statusRaw, ok := members[memberStatus]; ok {
		if statusRaw.Kind() != '0' {
			return Problem{}, fmt.Errorf("%w: status is not a number", ErrInvalid)
		}
		if err := json.Unmarshal(statusRaw, &p.Status); err != nil {
			return Problem{}, fmt.Errorf("%w: status: %w", ErrInvalid, err)
		}
	}
	for name, dst := range map[string]*string{memberType: &p.Type, memberTitle: &p.Title, memberDetail: &p.Detail} {
		if raw, ok := members[name]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return Problem{}, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
			}
		}
	}
	for name, raw := range members {
		switch name {
		case memberType, memberTitle, memberStatus, memberDetail:
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return Problem{}, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
		}
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra[name] = v
	}
	return p, nil
}
