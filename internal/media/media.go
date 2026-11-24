// Package media parses media types and ranges and negotiates a media type
// from Accept field values, as RFC 9110 §8.3 and §12.5.1 define them.
package media

import (
	"strings"
)

// Type is a media type or range: "application/json", "application/*+json",
// "text/*" or "*/*", lower-cased and without parameters.
type Type struct {
	Main, Sub string
}

// String returns t as "main/sub".
func (t Type) String() string { return t.Main + "/" + t.Sub }

// Parse reads a media type or range, ignoring parameters. ok is false when s
// is not of the form type/subtype.
func Parse(s string) (t Type, ok bool) {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	main, sub, found := strings.Cut(strings.TrimSpace(s), "/")
	main, sub = strings.ToLower(strings.TrimSpace(main)), strings.ToLower(strings.TrimSpace(sub))
	if !found || !token(main) || !token(sub) {
		return Type{}, false
	}
	if main == "*" && sub != "*" {
		return Type{}, false
	}
	if suffix, ok := strings.CutPrefix(sub, "*+"); ok && (suffix == "" || strings.Contains(suffix, "*")) {
		return Type{}, false
	}
	if strings.Contains(sub, "*") && sub != "*" && !strings.HasPrefix(sub, "*+") {
		return Type{}, false
	}
	return Type{Main: main, Sub: sub}, true
}

// Concrete reports whether t names one media type rather than a range.
func (t Type) Concrete() bool { return t.Main != "*" && !strings.Contains(t.Sub, "*") }

// Matches reports whether the range t includes the concrete type c. Besides
// "*/*" and "type/*", a range "type/*+suffix" includes every subtype with
// that structured syntax suffix (RFC 6839).
func (t Type) Matches(c Type) bool {
	switch {
	case t.Main == "*":
		return true
	case t.Main != c.Main:
		return false
	case t.Sub == "*" || t.Sub == c.Sub:
		return true
	}
	if suffix, ok := strings.CutPrefix(t.Sub, "*+"); ok {
		return strings.HasSuffix(c.Sub, "+"+suffix)
	}
	return false
}

// tchar holds the bytes RFC 9110 §5.6.2 allows in a token.
var tchar = func() (t [256]bool) {
	for c := '!'; c <= '~'; c++ {
		t[c] = !strings.ContainsRune(`()<>@,;:\"/[]?={}`, c)
	}
	return t
}()

func token(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !tchar[s[i]] {
			return false
		}
	}
	return true
}

type accepted struct {
	r Type
	q int // thousandths
}

// Negotiate returns the index of the offer to answer with, given the Accept
// field values of a request. Each offer is a concrete media type. An offer's
// quality comes from the most specific range that matches it, and the offer
// with the highest non-zero quality wins, ties going to the earlier offer.
// With no Accept field, or only empty ones, the first offer wins. ok is false
// when no offer is acceptable.
//
// When jsonAlias is set, an offer with the +json suffix is also matched by an
// "application/json" range, ranked below an exact match.
func Negotiate(accept []string, offers []Type, jsonAlias bool) (int, bool) {
	if len(offers) == 0 {
		return 0, false
	}
	if len(accept) == 1 && strings.TrimSpace(accept[0]) == "*/*" {
		return 0, true
	}
	var buf [8]accepted
	ranges, present := parseAccept(buf[:0], accept)
	if !present {
		return 0, true
	}
	best, bestQ := -1, 0
	for i, o := range offers {
		q := quality(ranges, o, jsonAlias)
		if q > bestQ {
			best, bestQ = i, q
		}
	}
	return best, best >= 0
}

func quality(ranges []accepted, o Type, jsonAlias bool) int {
	q, spec := 0, 0
	for _, a := range ranges {
		s := specificity(a.r, o, jsonAlias)
		if s > spec {
			q, spec = a.q, s
		}
	}
	return q
}

func specificity(r, o Type, jsonAlias bool) int {
	switch {
	case r.Main == o.Main && r.Sub == o.Sub:
		return 5
	case jsonAlias && r.Main == "application" && r.Sub == "json" && o.Main == "application" && strings.HasSuffix(o.Sub, "+json"):
		return 4
	case r.Main == o.Main && strings.HasPrefix(r.Sub, "*+") && r.Matches(o):
		return 3
	case r.Main == o.Main && r.Sub == "*":
		return 2
	case r.Main == "*":
		return 1
	}
	return 0
}

// parseAccept appends the ranges of values to out, so a caller can pass a
// stack buffer for the usual short Accept field.
func parseAccept(out []accepted, values []string) ([]accepted, bool) {
	present := false
	for _, v := range values {
		for member := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(member) == "" {
				continue
			}
			present = true
			mt, params, _ := strings.Cut(member, ";")
			t, ok := Parse(mt)
			if !ok {
				continue
			}
			q, ok := qvalue(params)
			if !ok {
				continue
			}
			out = append(out, accepted{r: t, q: q})
		}
	}
	return out, present
}

func qvalue(params string) (int, bool) {
	q := 1000
	for p := range strings.SplitSeq(params, ";") {
		name, value, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		v, ok := parseQ(strings.TrimSpace(value))
		if !ok {
			return 0, false
		}
		q = v
	}
	return q, true
}

// parseQ reads an RFC 9110 §12.4.2 qvalue: "0" or "1", optionally followed
// by "." and up to three digits, never above 1.
func parseQ(v string) (int, bool) {
	if v == "" || (v[0] != '0' && v[0] != '1') {
		return 0, false
	}
	whole, frac, dot := strings.Cut(v[1:], ".")
	if whole != "" || (dot && len(frac) > 3) || (!dot && len(v) > 1) {
		return 0, false
	}
	q := int(v[0]-'0') * 1000
	scale := 100
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, false
		}
		q += int(c-'0') * scale
		scale /= 10
	}
	if q > 1000 {
		return 0, false
	}
	return q, true
}
