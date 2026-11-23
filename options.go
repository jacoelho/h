package h

import (
	"errors"
	"fmt"
	"slices"

	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/problem"
)

// Option changes a setting for a group or a route. Settings resolve from the
// route outwards: the route's own, then its groups' from the innermost, then
// the router's Config.
type Option struct {
	apply     func(*level) error
	routeOnly string
}

// level holds the settings one router, group or route sets itself.
type level struct {
	mappers   []problem.Mapper
	refusals  []problem.Mapper
	formats   []problem.Format
	codecs    *codec.Set
	produces  []string
	consumes  []string
	maxBody   int64
	maxMemory int64
	spillAt   int
	use       []Middleware
	bypass    bool
}

// Errors adds m to the mappers that translate a route's errors into
// problems. Mappers closer to the route are tried first. They are never
// offered the requests the router refuses itself: see RequestErrors.
func Errors(m problem.Mapper) Option {
	return Option{apply: func(l *level) error {
		if err := m.Err(); err != nil {
			return fmt.Errorf("Errors: %w", err)
		}
		l.mappers = append(l.mappers, m)
		return nil
	}}
}

// RequestErrors adds m to the mappers that describe the requests the
// router refuses itself: a *RequestError, such as a malformed body (400) or
// an unsupported media type (415), and a *ParamsError. m is offered the
// error as the endpoint or router returned it, so it can match an error
// wrapped inside, such as a field error from a custom unmarshaler. It may
// answer any 4xx status; any other answer is reported as ErrInvalidProblem
// and the router's status stands. Mappers closer to the route are tried
// first.
func RequestErrors(m problem.Mapper) Option {
	return Option{apply: func(l *level) error {
		if err := m.Err(); err != nil {
			return fmt.Errorf("RequestErrors: %w", err)
		}
		l.refusals = append(l.refusals, m)
		return nil
	}}
}

// ProblemFormats sets the formats problems are written in, negotiated by
// Accept in the order given. The last one answers an Accept that matches
// none, so list problem.Text last to keep a plain-text answer for such
// clients.
func ProblemFormats(fs ...problem.Format) Option {
	return Option{apply: func(l *level) error {
		if len(fs) == 0 {
			return errors.New("ProblemFormats with no formats")
		}
		if slices.Contains(fs, nil) {
			return errors.New("ProblemFormats with a nil format")
		}
		l.formats = slices.Clone(fs)
		return nil
	}}
}

// Use wraps a group's or route's handlers with ms, the first outermost.
func Use(ms ...Middleware) Option {
	return Option{apply: func(l *level) error {
		if slices.ContainsFunc(ms, func(m Middleware) bool { return m == nil }) {
			return errors.New("Use with a nil middleware")
		}
		l.use = append(l.use, ms...)
		return nil
	}}
}

// Codecs sets the codecs a group's or route's endpoints negotiate between,
// in place of Config.Codecs. The closest setting wins; sets are not merged,
// so s must hold every codec the scope reads and writes. Produces and
// Consumes narrow it. Use it for a media type only some routes offer, which
// registered on Config.Codecs would be offered by every route.
func Codecs(s *codec.Set) Option {
	return Option{apply: func(l *level) error {
		if s == nil {
			return errors.New("Codecs with a nil set")
		}
		l.codecs = s
		return nil
	}}
}

// Produces narrows the media types a route writes to those matched by the
// given ranges.
func Produces(mediaRanges ...string) Option {
	return Option{apply: func(l *level) error { l.produces = slices.Clone(mediaRanges); return nil }}
}

// Consumes narrows the media types a route reads to those matched by the
// given ranges. A RawBody route must set it; "*/*" accepts any body whose
// Content-Type is a valid concrete media type, or that has none. A body
// without a Content-Type is matched as application/octet-stream.
func Consumes(mediaRanges ...string) Option {
	return Option{apply: func(l *level) error { l.consumes = slices.Clone(mediaRanges); return nil }}
}

// MaxBody bounds the request body in bytes.
func MaxBody(n int64) Option {
	return Option{apply: func(l *level) error { l.maxBody = n; return positive("MaxBody", n) }}
}

// MaxMemory bounds the multipart form data held in memory in bytes; larger
// files go to temporary files, removed when the request ends.
func MaxMemory(n int64) Option {
	return Option{apply: func(l *level) error { l.maxMemory = n; return positive("MaxMemory", n) }}
}

// SpillAt sets how many bytes of a response are buffered before it is
// streamed. A buffered response has a Content-Length, and a failure while
// encoding it is still answered as a problem.
func SpillAt(n int) Option {
	return Option{apply: func(l *level) error { l.spillAt = n; return positive("SpillAt", int64(n)) }}
}

func positive(name string, n int64) error {
	if n <= 0 {
		return fmt.Errorf("%s(%d) must be positive", name, n)
	}
	return nil
}

// InsecureAllowCrossOrigin exempts one route from cross-site request
// forgery protection, for endpoints other sites must reach, such as single
// sign-on callbacks. It is accepted only by route registrations.
func InsecureAllowCrossOrigin() Option {
	return Option{apply: func(l *level) error { l.bypass = true; return nil }, routeOnly: "InsecureAllowCrossOrigin"}
}

func levelOf(opts []Option, route bool) (level, error) {
	var l level
	for _, o := range opts {
		if o.apply == nil {
			return level{}, errors.New("zero Option")
		}
		if o.routeOnly != "" && !route {
			return level{}, fmt.Errorf("%s applies to routes only", o.routeOnly)
		}
		if err := o.apply(&l); err != nil {
			return level{}, err
		}
	}
	return l, nil
}
