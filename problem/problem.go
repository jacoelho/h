// Package problem describes HTTP API errors as problem details (RFC 9457),
// translates domain errors into them with mappers, and writes them in one of
// several wire formats.
//
// A Problem is the one representation of an error; how it is written is a
// Format, so an API can keep an error shape its clients already read.
package problem

import (
	"errors"
	"fmt"
	"github.com/jacoelho/h/internal/serverfield"
	"maps"
	"net/http"
	"net/url"
	"regexp"
)

// Problem is an HTTP error: its status, what kind of problem it is, and what
// a client may show or act on. It never carries internal error text.
type Problem struct {
	// Status is the HTTP status code, 400 to 599.
	Status int
	// Type identifies the kind of problem: an absolute URI, or "about:blank"
	// (the default when empty), which means the status says all there is.
	Type string
	// Title is a short summary of the kind of problem. When empty and Type is
	// "about:blank", the h router and the RFC 9457 and text formats use the
	// status's reason phrase.
	Title string
	// Detail explains this occurrence to a person.
	Detail string
	// Header holds response header fields to send with the problem, such as
	// WWW-Authenticate with a 401 (RFC 9110 §15.5.2).
	Header http.Header
	// Extra holds extension members. Keys start with a letter and contain
	// only letters, digits and underscores (RFC 9457 §3.2), and do not reuse
	// a standard member name.
	Extra map[string]any
}

// ErrInvalid reports a Problem that breaks the rules of its fields.
var ErrInvalid = errors.New("problem: invalid problem")

var extensionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// The members RFC 9457, section 3.1 defines, and the type of a problem
// that names no kind beyond its status.
const (
	memberType     = "type"
	memberTitle    = "title"
	memberStatus   = "status"
	memberDetail   = "detail"
	memberInstance = "instance"
	blankType      = "about:blank"
)

var reserved = map[string]bool{memberType: true, memberTitle: true, memberStatus: true, memberDetail: true, memberInstance: true}

// Validate reports whether p can be written: Status in 400 to 599, Type empty,
// "about:blank" or an absolute URI, no Header field the server owns
// (Content-Type, Content-Length, Access-Control-*), and every Extra key a
// valid extension name. The error wraps ErrInvalid.
func (p Problem) Validate() error {
	if p.Status < 400 || p.Status > 599 {
		return fmt.Errorf("%w: status %d is not 400 to 599", ErrInvalid, p.Status)
	}
	if p.Type != "" && p.Type != blankType {
		u, err := url.Parse(p.Type)
		if err != nil || !u.IsAbs() {
			return fmt.Errorf("%w: type %q is not an absolute URI", ErrInvalid, p.Type)
		}
	}
	for k := range p.Header {
		if ck := http.CanonicalHeaderKey(k); serverfield.Owned(ck) {
			return fmt.Errorf("%w: header %s belongs to the server", ErrInvalid, ck)
		}
	}
	for k := range p.Extra {
		if reserved[k] || !extensionName.MatchString(k) {
			return fmt.Errorf("%w: extension member %q", ErrInvalid, k)
		}
	}
	return nil
}

// Error returns an error carrying p, so an endpoint can answer with a
// problem no mapper needs to know. The h router answers it as it is, after
// Validate, without consulting mappers, even when wrapped.
func Error(p Problem) error { return &carried{p: p} }

// FromError returns the problem carried by an error from Error anywhere in
// err's tree, and whether there was one.
func FromError(err error) (Problem, bool) {
	c, ok := errors.AsType[*carried](err)
	if !ok {
		return Problem{}, false
	}
	return c.p, true
}

type carried struct{ p Problem }

func (c *carried) Error() string {
	s := fmt.Sprintf("problem: %d %s", c.p.Status, c.p.TitleOrStatus())
	if c.p.Detail != "" {
		s += ": " + c.p.Detail
	}
	return s
}

// With returns a copy of p with the extension member name set to v. p's
// own Extra map is left unchanged, so a Problem shared with others can be
// extended safely.
func (p Problem) With(name string, v any) Problem {
	extra := make(map[string]any, len(p.Extra)+1)
	maps.Copy(extra, p.Extra)
	extra[name] = v
	p.Extra = extra
	return p
}

// TypeOrBlank returns p.Type, or "about:blank" when it is empty.
func (p Problem) TypeOrBlank() string {
	if p.Type == "" {
		return blankType
	}
	return p.Type
}

// TitleOrStatus returns p.Title, or the status's reason phrase when the title
// is empty and the type is "about:blank".
func (p Problem) TitleOrStatus() string {
	if p.Title == "" && p.TypeOrBlank() == blankType {
		return http.StatusText(p.Status)
	}
	return p.Title
}

// Mapper translates errors into problems. The zero Mapper answers nothing.
// Build one with Is, As, Func or Map; mistakes found while building it are
// kept and reported by Err, so a router reports them at startup.
type Mapper struct {
	match func(error) (Problem, bool)
	err   error
}

// Problem returns the problem for err, and whether m knows err.
func (m Mapper) Problem(err error) (Problem, bool) {
	if m.match == nil {
		return Problem{}, false
	}
	return m.match(err)
}

// Err returns the mistakes found when m was built, or nil. A problem
// computed from the error at request time (As, Func) is checked only then.
func (m Mapper) Err() error { return m.err }

// Map returns a Mapper that tries ms in order and answers with the first
// that knows the error. Its Err joins the parts' errors.
func Map(ms ...Mapper) Mapper {
	errs := make([]error, 0, len(ms))
	for _, m := range ms {
		errs = append(errs, m.err)
	}
	return Mapper{err: errors.Join(errs...), match: func(err error) (Problem, bool) {
		for _, m := range ms {
			if p, ok := m.Problem(err); ok {
				return p, true
			}
		}
		return Problem{}, false
	}}
}

// Is returns a Mapper answering p for every error that matches target as
// errors.Is defines it. Its Err reports a nil target or an invalid p.
func Is(target error, p Problem) Mapper {
	if target == nil {
		return Mapper{err: fmt.Errorf("%w: Is with a nil target", ErrInvalid)}
	}
	return Mapper{err: p.Validate(), match: func(err error) (Problem, bool) {
		if errors.Is(err, target) {
			return p, true
		}
		return Problem{}, false
	}}
}

// As returns a Mapper answering f(e) for every error whose tree holds an E,
// as errors.AsType defines it. Its Err reports a nil f.
func As[E error](f func(E) Problem) Mapper {
	if f == nil {
		return Mapper{err: fmt.Errorf("%w: As with a nil function", ErrInvalid)}
	}
	return Mapper{match: func(err error) (Problem, bool) {
		if e, ok := errors.AsType[E](err); ok {
			return f(e), true
		}
		return Problem{}, false
	}}
}

// Func returns a Mapper calling f, a hand-written translator. Its Err
// reports a nil f.
func Func(f func(error) (Problem, bool)) Mapper {
	if f == nil {
		return Mapper{err: fmt.Errorf("%w: Func with a nil function", ErrInvalid)}
	}
	return Mapper{match: f}
}
