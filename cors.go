package h

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CORS configures cross-origin resource sharing (Fetch standard, CORS
// protocol) for Config.TrustedOrigins. The zero value is usable. The router
// never answers with "Access-Control-Allow-Origin: *" and never names an
// origin that is not trusted.
type CORS struct {
	// AllowHeaders are the request header fields a preflight allows; nil
	// means Content-Type and Authorization.
	AllowHeaders []string
	// ExposeHeaders are the response header fields the frontend may read.
	ExposeHeaders []string
	// AllowCredentials lets cross-origin requests carry cookies or HTTP
	// authentication.
	AllowCredentials bool
	// MaxAge is how long a browser may cache a preflight; 0 means 5 minutes.
	MaxAge time.Duration
}

func (c CORS) isZero() bool {
	return c.AllowHeaders == nil && c.ExposeHeaders == nil && !c.AllowCredentials && c.MaxAge == 0
}

type corsPolicy struct {
	allowHeaders string
	expose       string
	maxAge       string
	origins      []string
	credentials  bool
}

func newCORSPolicy(origins []string, c CORS) (corsPolicy, error) {
	if len(origins) == 0 {
		if !c.isZero() {
			return corsPolicy{}, errors.New("h: CORS is configured but TrustedOrigins is empty, so it can never apply")
		}
		return corsPolicy{}, nil
	}
	allow := c.AllowHeaders
	if allow == nil {
		allow = []string{"Content-Type", "Authorization"}
	}
	if c.MaxAge < 0 || (c.MaxAge > 0 && c.MaxAge < time.Second) {
		return corsPolicy{}, fmt.Errorf("h: CORS.MaxAge %v must be zero or at least one second", c.MaxAge)
	}
	maxAge := c.MaxAge
	if maxAge == 0 {
		maxAge = 5 * time.Minute
	}
	return corsPolicy{
		origins:      slices.Clone(origins),
		allowHeaders: strings.Join(allow, ", "),
		expose:       strings.Join(c.ExposeHeaders, ", "),
		credentials:  c.AllowCredentials,
		maxAge:       strconv.Itoa(int(maxAge / time.Second)),
	}, nil
}

// wrap answers preflights and adds CORS fields to responses for trusted
// origins. It sits outside root middleware, so preflights never reach
// authentication and responses produced by root middleware stay readable.
func (p corsPolicy) wrap(next http.Handler, c *core) http.Handler {
	if len(p.origins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		addVary(h, "Origin")
		origin := req.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, req)
			return
		}
		trusted := slices.Contains(p.origins, origin)
		if req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" {
			addVary(h, "Access-Control-Request-Method, Access-Control-Request-Headers")
			if trusted && c.hasRoute(req, req.Header.Get("Access-Control-Request-Method")) {
				p.allow(h, origin)
				h.Set("Access-Control-Allow-Methods", strings.Join(c.allowed(req), ", "))
				h.Set("Access-Control-Allow-Headers", p.allowHeaders)
				h.Set("Access-Control-Max-Age", p.maxAge)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if trusted {
			p.allow(h, origin)
			if p.expose != "" {
				h.Set("Access-Control-Expose-Headers", p.expose)
			}
		}
		next.ServeHTTP(w, req)
	})
}

func (p corsPolicy) allow(h http.Header, origin string) {
	h.Set("Access-Control-Allow-Origin", origin)
	if p.credentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
}
