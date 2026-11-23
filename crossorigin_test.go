package h_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

func created(called *int) h.Endpoint[h.Empty, h.Empty] {
	return func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		*called++
		return h.Status(http.StatusCreated, h.Empty{}), nil
	}
}

func TestCrossOriginProtection(t *testing.T) {
	cases := []struct {
		name   string
		method string
		header http.Header
		status int
	}{
		{"cross-site POST", "POST", http.Header{"Sec-Fetch-Site": {"cross-site"}}, 403},
		{"same-origin POST", "POST", http.Header{"Sec-Fetch-Site": {"same-origin"}}, 201},
		{"user-initiated POST", "POST", http.Header{"Sec-Fetch-Site": {"none"}}, 201},
		{"Origin host differs", "POST", http.Header{"Origin": {"https://evil.example"}}, 403},
		{"Origin null", "POST", http.Header{"Origin": {"null"}}, 403},
		{"no browser headers", "POST", nil, 201},
		{"cross-site GET", "GET", http.Header{"Sec-Fetch-Site": {"cross-site"}}, 200},
		{"trusted origin", "POST", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://app.example"}}, 201},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log htest.Errors
			var called int
			r := newRouter(t, h.Config{OnError: log.OnError, TrustedOrigins: []string{"https://app.example"}})
			r.Post("/orders", created(&called))
			r.Get("/orders", respond(h.OK(item{"a"})))
			w := send(t, r, call{method: c.method, target: "http://api.example/orders", header: c.header})
			w.Expect(t, c.status)
			if c.status == 403 {
				w.Problem(t)
				if called != 0 {
					t.Fatal("endpoint called")
				}
				if !errors.Is(log.Last(t).Err, h.ErrCrossOrigin) {
					t.Fatalf("OnError got %v", log.Last(t).Err)
				}
			}
		})
	}
}

func TestTrustedOriginNeedsScheme(t *testing.T) {
	if _, err := h.New(h.Config{TrustedOrigins: []string{"example.com"}}); err == nil {
		t.Fatal("origin without scheme accepted")
	}
}

func TestInsecureAllowCrossOriginIsPerRoute(t *testing.T) {
	var called int
	r := newRouter(t, h.Config{})
	g := r.Group("/sso")
	g.Post("/callback", created(&called), h.InsecureAllowCrossOrigin())
	g.Post("/other", created(&called))
	r.Handle("POST", "/raw", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusCreated)
		return nil
	})
	cross := http.Header{"Sec-Fetch-Site": {"cross-site"}}
	send(t, r, call{method: "POST", target: "/sso/callback", header: cross}).Expect(t, 201)
	send(t, r, call{method: "POST", target: "/sso/other", header: cross}).Expect(t, 403)
	send(t, r, call{method: "POST", target: "/raw", header: cross}).Expect(t, 403)
}

func TestRootMiddlewareRecordsRefusal(t *testing.T) {
	r := newRouter(t, h.Config{})
	var status int
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, req)
			status = sw.status
		})
	})
	var called int
	r.Post("/x", created(&called))
	send(t, r, call{method: "POST", target: "/x", header: http.Header{"Sec-Fetch-Site": {"cross-site"}}})
	if status != 403 {
		t.Fatalf("root middleware saw %d", status)
	}
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func corsRouter(t *testing.T, called *int) *h.Router {
	t.Helper()
	r := newRouter(t, h.Config{
		TrustedOrigins: []string{"https://app.example"},
		CORS:           h.CORS{AllowHeaders: []string{"Content-Type"}, ExposeHeaders: []string{"Location"}, AllowCredentials: true, MaxAge: time.Minute},
	})
	r.Put("/items/{id}", func(context.Context, h.Request[h.Empty]) (h.Response[h.Empty], error) {
		*called++
		return h.Response[h.Empty]{}, nil
	})
	r.Post("/items", created(called))
	r.Fallback()
	return r
}

func preflight(origin, method, headers string) call {
	hdr := http.Header{"Origin": {origin}, "Access-Control-Request-Method": {method}}
	if headers != "" {
		hdr.Set("Access-Control-Request-Headers", headers)
	}
	return call{method: "OPTIONS", target: "http://api.example/items/1", header: hdr}
}

func corsFields(w http.Header) []string {
	var out []string
	for k := range w {
		if strings.HasPrefix(k, "Access-Control-") {
			out = append(out, k)
		}
	}
	return out
}

func TestCORSPreflight(t *testing.T) {
	var called int
	r := corsRouter(t, &called)

	w := send(t, r, preflight("https://app.example", "PUT", "Content-Type, X-Secret"))
	w.Expect(t, http.StatusNoContent)
	hdr := w.Header()
	if hdr.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		!strings.Contains(hdr.Get("Access-Control-Allow-Methods"), "PUT") ||
		hdr.Get("Access-Control-Max-Age") != "60" ||
		hdr.Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(strings.Join(hdr.Values("Vary"), ","), "Origin") {
		t.Fatalf("preflight header %v", hdr)
	}
	if allow := hdr.Get("Access-Control-Allow-Headers"); strings.Contains(allow, "X-Secret") {
		t.Fatalf("unlisted request header echoed: %q", allow)
	}
	if called != 0 {
		t.Fatal("endpoint called by a preflight")
	}

	for name, c := range map[string]call{
		"untrusted origin":     preflight("https://evil.example", "PUT", ""),
		"method with no route": preflight("https://app.example", "DELETE", ""),
	} {
		w := send(t, r, c)
		w.Expect(t, http.StatusNoContent)
		if f := corsFields(w.Header()); len(f) != 0 {
			t.Fatalf("%s: CORS fields %v", name, f)
		}
	}
}

func TestCORSActualRequests(t *testing.T) {
	var called int
	r := corsRouter(t, &called)
	origin := http.Header{"Origin": {"https://app.example"}, "Sec-Fetch-Site": {"cross-site"}}

	w := send(t, r, call{method: "POST", target: "http://api.example/items", header: origin})
	w.Expect(t, http.StatusCreated)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" || w.Header().Get("Access-Control-Expose-Headers") != "Location" {
		t.Fatalf("header %v", w.Header())
	}

	w = send(t, r, call{method: "GET", target: "http://api.example/nope", header: origin})
	w.Expect(t, http.StatusNotFound)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("problem from a trusted origin is unreadable by it")
	}

	w = send(t, r, call{method: "GET", target: "http://api.example/nope", header: http.Header{"Origin": {"https://evil.example"}}})
	if f := corsFields(w.Header()); len(f) != 0 {
		t.Fatalf("untrusted origin got %v", f)
	}
}

func TestCORSIsOutsideRootMiddleware(t *testing.T) {
	var called int
	r := newRouter(t, h.Config{TrustedOrigins: []string{"https://app.example"}})
	r.Use(requireAuth)
	r.Post("/items", created(&called))

	w := send(t, r, call{method: "OPTIONS", target: "http://api.example/items", header: http.Header{
		"Origin": {"https://app.example"}, "Access-Control-Request-Method": {"POST"},
	}})
	w.Expect(t, http.StatusNoContent)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("preflight did not reach CORS before authentication")
	}

	w = send(t, r, call{method: "POST", target: "http://api.example/items", header: http.Header{"Origin": {"https://app.example"}}})
	w.Expect(t, http.StatusUnauthorized)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("401 from root middleware lacks CORS fields")
	}
}

func TestCORSWithoutOriginsIsAnError(t *testing.T) {
	if _, err := h.New(h.Config{CORS: h.CORS{AllowCredentials: true}}); err == nil {
		t.Fatal("CORS without trusted origins accepted")
	}
}

func TestProblemVaryMergesWithCORS(t *testing.T) {
	errLimited := errors.New("limited")
	r := newRouter(t, h.Config{
		TrustedOrigins: []string{"https://app.example"},
		Errors:         problem.Is(errLimited, problem.Problem{Status: 429, Header: http.Header{"Vary": {"Authorization"}}}),
	})
	r.Get("/x", failing(errLimited))
	w := send(t, r, call{method: "GET", target: "http://api.example/x", header: http.Header{"Origin": {"https://app.example"}}})
	w.Expect(t, http.StatusTooManyRequests)
	got := strings.Join(w.Header().Values("Vary"), ",")
	for _, want := range []string{"Origin", "Accept", "Authorization"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Vary %q lacks %s", got, want)
		}
	}
}
