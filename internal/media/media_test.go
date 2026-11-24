package media

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func mustParse(s string) Type {
	t, ok := Parse(s)
	if !ok {
		panic("malformed media type in test: " + s)
	}
	return t
}

func types(names ...string) []Type {
	out := make([]Type, len(names))
	for i, n := range names {
		out[i] = mustParse(n)
	}
	return out
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		name      string
		accept    []string
		offers    []string
		jsonAlias bool
		want      string
	}{
		{"no Accept takes the first offer", nil, []string{"application/json", "text/plain"}, false, "application/json"},
		{"empty Accept takes the first offer", []string{""}, []string{"application/json", "text/plain"}, false, "application/json"},
		{"higher q wins over order", []string{"application/xml;q=0.9, application/json"}, []string{"application/xml", "application/json"}, false, "application/json"},
		{"most specific range sets q=0", []string{"*/*, application/json;q=0"}, []string{"application/json"}, false, ""},
		{"q=0 excludes only the named type", []string{"*/*, application/json;q=0"}, []string{"application/json", "text/plain"}, false, "text/plain"},
		{"fields across lines are all read", []string{"application/xml;q=0.1", "application/json"}, []string{"application/xml", "application/json"}, false, "application/json"},
		{"type wildcard", []string{"text/*"}, []string{"application/json", "text/plain"}, false, "text/plain"},
		{"suffix range", []string{"application/*+json"}, []string{"text/plain", "application/problem+json"}, false, "application/problem+json"},
		{"tie keeps offer order", []string{"*/*"}, []string{"text/plain", "application/json"}, false, "text/plain"},
		{"json alias matches +json", []string{"application/json"}, []string{"application/problem+json", "text/plain"}, true, "application/problem+json"},
		{"no alias without the flag", []string{"application/json"}, []string{"application/problem+json"}, false, ""},
		{"exact beats alias", []string{"application/json;q=0.5, application/problem+json;q=0"}, []string{"application/problem+json"}, true, ""},
		{"malformed members are skipped", []string{"garbage, text/plain;q=2, application/json"}, []string{"text/plain", "application/json"}, false, "application/json"},
		{"case-insensitive", []string{"Application/JSON"}, []string{"application/json"}, false, "application/json"},
		{"nothing acceptable", []string{"image/png"}, []string{"application/json"}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			offers := types(c.offers...)
			i, ok := Negotiate(c.accept, offers, c.jsonAlias)
			got := ""
			if ok {
				got = offers[i].String()
			}
			if got != c.want {
				t.Fatalf("Negotiate(%q, %v) = %q, want %q", c.accept, c.offers, got, c.want)
			}
		})
	}
}

// model is RFC 9110 §12.5.1 stated directly: an offer's quality is that of
// the most specific matching range (exact, type/*, */*), and the best
// non-zero quality wins, ties to the earlier offer.
func model(ranges []accepted, offers []Type) (int, bool) {
	best, bestQ := -1, 0
	for i, o := range offers {
		q, rank := 0, 0
		for _, r := range ranges {
			k := 0
			switch {
			case r.r == o:
				k = 3
			case r.r.Main == o.Main && r.r.Sub == "*":
				k = 2
			case r.r.Main == "*":
				k = 1
			}
			if k > rank {
				q, rank = r.q, k
			}
		}
		if q > bestQ {
			best, bestQ = i, q
		}
	}
	return best, best >= 0
}

func TestNegotiateMatchesModel(t *testing.T) {
	seed := rand.Uint64()
	rng := rand.New(rand.NewPCG(seed, 0))
	pool := []string{"application/json", "application/xml", "text/plain", "text/html", "text/*", "application/*", "*/*"}
	concrete := []string{"application/json", "application/xml", "text/plain", "text/html"}
	for range 5000 {
		var members []string
		var ranges []accepted
		seen := map[Type]bool{}
		for range 1 + rng.IntN(4) {
			r := mustParse(pool[rng.IntN(len(pool))])
			if seen[r] {
				continue
			}
			seen[r] = true
			q := rng.IntN(4) * 333
			members = append(members, fmt.Sprintf("%s;q=0.%03d", r, q))
			ranges = append(ranges, accepted{r: r, q: q})
		}
		var offers []Type
		for _, i := range rng.Perm(len(concrete))[:1+rng.IntN(len(concrete))] {
			offers = append(offers, mustParse(concrete[i]))
		}
		accept := []string{strings.Join(members, ", ")}
		gi, gok := Negotiate(accept, offers, false)
		wi, wok := model(ranges, offers)
		if gi != wi && (gok || wok) || gok != wok {
			t.Fatalf("seed %d: Accept %q offers %v: got (%d,%v), model (%d,%v)", seed, accept, offers, gi, gok, wi, wok)
		}
	}
}

func TestParseRejectsMalformedWildcards(t *testing.T) {
	for _, s := range []string{"text/*foo", "text/a*", "text/*+*", "text/*+", "*/json"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
	for _, s := range []string{"text/*", "application/*+json", "*/*"} {
		if _, ok := Parse(s); !ok {
			t.Errorf("Parse(%q) rejected", s)
		}
	}
}

func TestParseRefusesNonTokens(t *testing.T) {
	for _, s := range []string{"/json", "application/", "app@lication/json", "application/js on", "application/json/x"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestQvalueFollowsRFC9110Grammar(t *testing.T) {
	valid := map[string]int{"0": 0, "1": 1000, "0.5": 500, "0.123": 123, "1.000": 1000, "1.": 1000, "0.": 0}
	for v, want := range valid {
		if got, ok := parseQ(v); !ok || got != want {
			t.Errorf("parseQ(%q) = %d, %v; want %d", v, got, ok, want)
		}
	}
	for _, v := range []string{"", "1e-1", "+.5", ".5", "NaN", "0.1234", "1.001", "2", "01", "0.5x", "0.0a", "0.:"} {
		if _, ok := parseQ(v); ok {
			t.Errorf("parseQ(%q) accepted", v)
		}
	}
}

func BenchmarkNegotiateBrowserAccept(b *testing.B) {
	accept := []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"}
	offers := types("application/json", "text/plain")
	b.ReportAllocs()
	for b.Loop() {
		if i, ok := Negotiate(accept, offers, false); !ok || i != 0 {
			b.Fatal(i, ok)
		}
	}
}

func TestNegotiateTakesTheLowestNonZeroQuality(t *testing.T) {
	if i, ok := Negotiate([]string{"text/plain;q=0.001"}, types("text/plain"), false); !ok || i != 0 {
		t.Fatalf("Negotiate with q=0.001 = %d, %v; want 0, true", i, ok)
	}
}

func TestNegotiateSkipsAMemberWithAMalformedQuality(t *testing.T) {
	if i, ok := Negotiate([]string{"application/json;q=high, */*"}, types("application/json"), false); !ok || i != 0 {
		t.Fatalf("Negotiate = %d, %v; want the */* member to accept application/json", i, ok)
	}
}
