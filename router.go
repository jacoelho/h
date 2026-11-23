package h

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/problem"
)

// Middleware wraps a handler with behaviour of its own.
type Middleware func(http.Handler) http.Handler

func chain(ms []Middleware, h http.Handler) http.Handler {
	for _, m := range slices.Backward(ms) {
		h = m(h)
	}
	return h
}

// Config holds the settings of a router. The zero Config is usable.
type Config struct {
	// BasePath is where the router lives, such as "/sous-chef". Every route,
	// group, Fallback and Request.Link is relative to it. Empty means "/".
	BasePath string
	// Errors translates errors no route or group mapper knows. The zero
	// Mapper knows none.
	Errors problem.Mapper
	// Codecs are the codecs routes negotiate between; nil means
	// codec.Default().
	Codecs *codec.Set
	// ProblemFormats are the formats problems are written in; nil means
	// problem.RFC9457. problem.Text is always the last resort.
	ProblemFormats []problem.Format
	// OnError observes every error answered as a problem. It must not write
	// to the response or panic (see OnError).
	OnError OnError
	// MaxBody bounds request bodies in bytes; 0 means 1 MiB.
	MaxBody int64
	// MaxMemory bounds multipart form data held in memory; 0 means 1 MiB.
	MaxMemory int64
	// SpillAt is how many response bytes are buffered before streaming;
	// 0 means 64 KiB.
	SpillAt int
	// TrustedOrigins are the browser origins, "scheme://host[:port]", that
	// may send unsafe cross-origin requests and are allowed by CORS.
	TrustedOrigins []string
	// CORS configures responses to the trusted origins.
	CORS CORS
}

// ErrSetup reports a mistake in registering routes, groups, middleware or
// options. Router.Handler returns every such mistake joined, each wrapping
// ErrSetup.
var ErrSetup = errors.New("h: invalid setup")

// Router collects routes, groups and middleware, and builds the handler
// that serves them with Handler. Registration records mistakes instead of
// failing at once; Handler reports them all. A Router is not safe for
// concurrent use.
type Router struct {
	core   *core
	parent *Router
	prefix string
	level  level
	broken bool
}

type core struct {
	base     string
	cfg      Config
	codecs   *codec.Set
	csrf     *http.CrossOriginProtection
	cors     corsPolicy
	resolver *resolver
	use      []Middleware
	routes   []pending
	errs     []error
	served   *served
}

// served is what Handler built from the registrations. A core is
// registering while it is nil and built once it is set; nothing changes it
// afterwards.
type served struct {
	handler http.Handler
	mux     *http.ServeMux
	methods []string // registered methods, HEAD with GET, for Allow fields
}

// pending is a registration waiting for Handler, which resolves its
// settings from every level once all middleware and options are known.
type pending struct {
	r        *Router
	method   string
	path     string
	own      level
	fallback bool
	build    func(res resolved, pattern string) (http.Handler, error)
}

// New returns a Router for cfg, or an error naming a setting that is invalid.
func New(cfg Config) (*Router, error) {
	base, err := basePath(cfg.BasePath)
	if err != nil {
		return nil, err
	}
	if err := cfg.Errors.Err(); err != nil {
		return nil, fmt.Errorf("h: Config.Errors: %w", err)
	}
	if cfg.Codecs == nil {
		cfg.Codecs = codec.Default()
	}
	if cfg.ProblemFormats == nil {
		cfg.ProblemFormats = []problem.Format{problem.RFC9457}
	}
	for name, v := range map[string]int64{"MaxBody": cfg.MaxBody, "MaxMemory": cfg.MaxMemory, "SpillAt": int64(cfg.SpillAt)} {
		if v < 0 {
			return nil, fmt.Errorf("h: %s is %d; it must not be negative", name, v)
		}
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.MaxMemory == 0 {
		cfg.MaxMemory = 1 << 20
	}
	if cfg.SpillAt == 0 {
		cfg.SpillAt = 64 << 10
	}
	c := &core{
		base:   base,
		cfg:    cfg,
		codecs: cfg.Codecs,
		csrf:   http.NewCrossOriginProtection(),
	}
	for _, o := range cfg.TrustedOrigins {
		if err := c.csrf.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("h: trusted origin %q: %w", o, err)
		}
	}
	if c.cors, err = newCORSPolicy(cfg.TrustedOrigins, cfg.CORS); err != nil {
		return nil, err
	}
	if c.resolver, err = newResolver(rootMappers(cfg), cfg.ProblemFormats, cfg.OnError); err != nil {
		return nil, err
	}
	return &Router{core: c}, nil
}

func rootMappers(cfg Config) []problem.Mapper { return []problem.Mapper{cfg.Errors} }

func basePath(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "{}") || path.Clean(p) != p {
		return "", fmt.Errorf("h: base path %q must start with /, be clean and hold no wildcards", p)
	}
	return p, nil
}

func (c *core) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("%w: "+format, append([]any{ErrSetup}, args...)...))
}

// Group returns a router for the routes under prefix, such as "/orders",
// with opts applied to all of them. A prefix may hold wildcards. An invalid
// prefix or option is reported by Handler, and the group's routes are not
// registered.
func (r *Router) Group(prefix string, opts ...Option) *Router {
	g := &Router{core: r.core, parent: r, prefix: r.prefix + prefix, broken: true}
	if !r.mutable(fmt.Sprintf("Group %q created", g.prefix)) {
		return g
	}
	if !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || path.Clean(prefix) != prefix {
		r.core.fail("group prefix %q must start with /, not end with /, and be clean", prefix)
		return g
	}
	l, err := levelOf(opts, false)
	if err != nil {
		r.core.fail("group %q: %v", g.prefix, err)
		return g
	}
	g.level, g.broken = l, false
	return g
}

// Use wraps handlers with ms, the first outermost. On the root router it
// wraps every request, including those no route matches; on a group it
// wraps all the group's routes, whether registered before or after.
func (r *Router) Use(ms ...Middleware) {
	if !r.mutable("Use") {
		return
	}
	if slices.ContainsFunc(ms, func(m Middleware) bool { return m == nil }) {
		r.core.fail("Use with a nil middleware")
		return
	}
	if r.parent == nil {
		r.core.use = append(r.core.use, ms...)
		return
	}
	r.level.use = append(r.level.use, ms...)
}

// mutable reports whether r may still change. A change after Handler is
// recorded as a mistake; a change to a broken group is dropped, its mistake
// being recorded already. Every exported mutator asks first.
func (r *Router) mutable(change string) bool {
	if r.broken {
		return false
	}
	if r.core.served != nil {
		r.core.fail("%s after Handler", change)
		return false
	}
	return true
}

// Mount returns the pattern an outer ServeMux mounts the router at, such as
// "/sous-chef/".
func (r *Router) Mount() string { return r.core.base + "/" }

// Handler builds the handler serving every registered route, or returns
// every setup mistake, joined and each wrapping ErrSetup. The handler
// answers CORS first, then runs the root middleware, then matches the
// route, which checks cross-site request forgery before its group
// middleware and endpoint; it is safe for concurrent use. Handler builds once: later calls return the same result, and
// any change made after the first call (a route, group, middleware or
// fallback) is not applied and is reported as a mistake by later calls.
func (r *Router) Handler() (http.Handler, error) {
	c := r.core
	if c.served == nil {
		c.served = c.build()
	}
	if len(c.errs) > 0 {
		return nil, errors.Join(c.errs...)
	}
	return c.served.handler, nil
}

func (c *core) build() *served {
	mux := http.NewServeMux()
	for _, p := range c.routes {
		c.install(mux, p)
	}
	methods := registeredMethods(c.routes)
	c.routes = nil
	return &served{handler: c.wrap(mux), mux: mux, methods: methods}
}

// registeredMethods lists the methods routes were registered for, the
// common ones in their usual order and any others after them, sorted. GET
// brings HEAD, which ServeMux answers with the GET route.
func registeredMethods(routes []pending) []string {
	seen := map[string]bool{}
	for _, p := range routes {
		if !p.fallback {
			seen[p.method] = true
		}
	}
	if seen[http.MethodGet] {
		seen[http.MethodHead] = true
	}
	var out []string
	for _, m := range routeMethods {
		if seen[m] {
			out = append(out, m)
			delete(seen, m)
		}
	}
	return append(out, slices.Sorted(maps.Keys(seen))...)
}

// wrap puts the router-wide handlers around mux: CORS, then the root
// middleware.
func (c *core) wrap(mux *http.ServeMux) http.Handler {
	inner := chain(c.use, http.Handler(mux))
	if len(c.use) == 0 {
		return c.cors.wrap(inner, c)
	}
	withPattern := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if h, p := mux.Handler(req); isRoute(h) {
			req.Pattern = p
		}
		inner.ServeHTTP(w, req)
	})
	return c.cors.wrap(withPattern, c)
}

func (c *core) install(mux *http.ServeMux, p pending) {
	res, err := p.r.resolve(p.own)
	if err != nil {
		c.fail("%s %s: %v", p.method, p.path, err)
		return
	}
	var patterns []string
	if p.fallback {
		patterns = append(patterns, c.base+p.r.prefix+"/")
		if exact := c.base + p.r.prefix; exact != "" {
			patterns = append(patterns, exact)
		}
	} else {
		full, err := p.r.full(p.path)
		if err != nil {
			c.fail("%s %q: %v", p.method, p.path, err)
			return
		}
		patterns = append(patterns, p.method+" "+full)
	}
	h, err := p.build(res, patterns[0])
	if err != nil {
		c.fail("route %q: %v", patterns[0], err)
		return
	}
	h = chain(res.use, h)
	if !res.bypass {
		h = c.protect(h, res.resolver)
	}
	h = route{Handler: h, fallback: p.fallback}
	for _, pattern := range patterns {
		if err := handle(mux, pattern, h); err != nil {
			c.fail("%v", err)
			return
		}
	}
}

// protect refuses an unsafe cross-site request before h runs, answering
// through the route's own resolver. Deciding per matched route, rather than
// by bypass patterns, keeps a route's exemption from covering other routes
// its pattern overlaps.
func (c *core) protect(h http.Handler, rs *resolver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := c.csrf.Check(req); err != nil {
			rs.fail(w, req, &RequestError{Status: http.StatusForbidden, Err: fmt.Errorf("%w: %w", ErrCrossOrigin, err)})
			return
		}
		h.ServeHTTP(w, req)
	})
}

// handle registers pattern on mux. ServeMux.Handle reports an invalid or
// conflicting pattern only by panicking (net/http documentation of
// ServeMux.Handle); this turns that into an error.
func handle(mux *http.ServeMux, pattern string, h http.Handler) error {
	return catch(func() { mux.Handle(pattern, h) })
}

func catch(f func()) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%v", v)
		}
	}()
	f()
	return nil
}

// hasRoute reports whether req's path has a route for method, ignoring
// fallbacks.
func (c *core) hasRoute(req *http.Request, method string) bool {
	probe := *req
	probe.Method = method
	h, _ := c.served.mux.Handler(&probe)
	rt, ok := h.(route)
	return ok && !rt.fallback
}

// route marks the handlers the router registers, so a lookup can tell a
// matched route from ServeMux's own redirects and errors, which also report
// a pattern, and a route from a fallback.
type route struct {
	http.Handler
	fallback bool
}

func isRoute(h http.Handler) bool {
	_, ok := h.(route)
	return ok
}

var routeMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

func (c *core) allowed(req *http.Request) []string {
	var out []string
	for _, m := range c.served.methods {
		if c.hasRoute(req, m) {
			out = append(out, m)
		}
	}
	return out
}

type resolved struct {
	resolver  *resolver
	produces  []string
	consumes  []string
	maxBody   int64
	maxMemory int64
	spillAt   int
	use       []Middleware
	bypass    bool
}

func (r *Router) resolve(own level) (resolved, error) {
	cfg := r.core.cfg
	res := resolved{maxBody: cfg.MaxBody, maxMemory: cfg.MaxMemory, spillAt: cfg.SpillAt}
	var levels []level
	for x := r; x != nil; x = x.parent {
		levels = append(levels, x.level)
	}
	slices.Reverse(levels)
	levels = append(levels, own)
	var mappers []problem.Mapper
	formats := cfg.ProblemFormats
	for _, l := range levels {
		mappers = append(slices.Clone(l.mappers), mappers...)
		if l.formats != nil {
			formats = l.formats
		}
		if l.produces != nil {
			res.produces = l.produces
		}
		if l.consumes != nil {
			res.consumes = l.consumes
		}
		if l.maxBody != 0 {
			res.maxBody = l.maxBody
		}
		if l.maxMemory != 0 {
			res.maxMemory = l.maxMemory
		}
		if l.spillAt != 0 {
			res.spillAt = l.spillAt
		}
		res.use = append(res.use, l.use...)
		res.bypass = l.bypass
	}
	mappers = append(mappers, cfg.Errors)
	rs, err := newResolver(mappers, formats, cfg.OnError)
	if err != nil {
		return resolved{}, err
	}
	res.resolver = rs
	return res, nil
}

func (r *Router) full(p string) (string, error) {
	if (p == "" || p == "/") && r.parent != nil {
		return r.core.base + r.prefix, nil
	}
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("route path %q must start with /", p)
	}
	full := r.core.base + r.prefix + p
	if strings.HasSuffix(full, "/") {
		full += "{$}"
	}
	return full, nil
}

// register records a route; callers have asked mutable first.
func (r *Router) register(method, p string, opts []Option, build func(res resolved, pattern string) (http.Handler, error)) {
	c := r.core
	if method == "" || strings.ContainsAny(method, " \t") {
		c.fail("invalid method %q", method)
		return
	}
	own, err := levelOf(opts, true)
	if err != nil {
		c.fail("%s %q: %v", method, p, err)
		return
	}
	c.routes = append(c.routes, pending{r: r, method: method, path: p, own: own, build: build})
}

// Get registers a GET (and HEAD) route. GET has no request body.
func (r *Router) Get[Out any](path string, e Endpoint[Empty, Out], opts ...Option) {
	registerEndpoint(r, http.MethodGet, path, e, opts)
}

// Delete registers a DELETE route. DELETE has no request body.
func (r *Router) Delete[Out any](path string, e Endpoint[Empty, Out], opts ...Option) {
	registerEndpoint(r, http.MethodDelete, path, e, opts)
}

// Post registers a POST route.
func (r *Router) Post[In, Out any](path string, e Endpoint[In, Out], opts ...Option) {
	registerEndpoint(r, http.MethodPost, path, e, opts)
}

// Put registers a PUT route.
func (r *Router) Put[In, Out any](path string, e Endpoint[In, Out], opts ...Option) {
	registerEndpoint(r, http.MethodPut, path, e, opts)
}

// Patch registers a PATCH route.
func (r *Router) Patch[In, Out any](path string, e Endpoint[In, Out], opts ...Option) {
	registerEndpoint(r, http.MethodPatch, path, e, opts)
}

// Handle registers a route served by a handler that writes the response
// itself, such as one calling http.ServeFile. An error it returns before
// writing is answered as a problem through the route's mappers; after
// writing, the connection is aborted. A panic is answered like an
// endpoint's. The writer it receives implements http.Flusher and
// http.Hijacker, and supports http.ResponseController; an error returned
// after a flush or hijack is reported to OnError as ErrEncodeAfterCommit
// and nothing more is written.
func (r *Router) Handle(method, path string, fn func(http.ResponseWriter, *http.Request) error, opts ...Option) {
	if !r.mutable(fmt.Sprintf("%s %q registered", method, path)) {
		return
	}
	if fn == nil {
		r.core.fail("%s %q: nil handler", method, path)
		return
	}
	r.register(method, path, opts, func(res resolved, _ string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			tw := &trackingWriter{ResponseWriter: w}
			req.Body = newBodyReader(w, req, res.maxBody)
			if err := contain(func() error { return fn(tw, req) }); err != nil {
				tw.answer(req, res.resolver, err)
			}
		}), nil
	})
}

// Raw adapts a handler that reports no errors for Handle.
func Raw(h http.Handler) func(http.ResponseWriter, *http.Request) error {
	if h == nil {
		return nil
	}
	return func(w http.ResponseWriter, req *http.Request) error {
		h.ServeHTTP(w, req)
		return nil
	}
}

var (
	errNoRoute          = errors.New("h: no route")
	errMethodNotAllowed = errors.New("h: method not allowed")
)

// Fallback answers every request under the router's prefix that no route
// matches: 405 with Allow when the path has routes for other methods,
// otherwise 404, both as problems.
func (r *Router) Fallback() {
	c := r.core
	if !r.mutable("Fallback registered") {
		return
	}
	c.routes = append(c.routes, pending{r: r, method: "*", path: r.prefix + "/", fallback: true,
		build: func(res resolved, _ string) (http.Handler, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if methods := c.allowed(req); len(methods) > 0 {
					p, report := res.resolver.resolve(req, &RequestError{Status: http.StatusMethodNotAllowed, Err: errMethodNotAllowed})
					p.Header = p.Header.Clone()
					if p.Header == nil {
						p.Header = http.Header{}
					}
					p.Header.Set("Allow", strings.Join(methods, ", "))
					res.resolver.observe(req, report, p)
					res.resolver.write(w, req, p)
					return
				}
				res.resolver.fail(w, req, &RequestError{Status: http.StatusNotFound, Err: errNoRoute})
			}), nil
		}})
}

// trackingWriter is the one owner of whether a response has started: every
// write, final status, successful flush and hijack goes through it.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

// WriteHeader sends the status. An informational status (1xx other than
// 101) leaves the response unstarted, since a final status may follow.
func (t *trackingWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		t.wrote = true
	}
	t.ResponseWriter.WriteHeader(code)
}

// answer reports err: as a problem while nothing was sent, otherwise by
// aborting the connection.
func (t *trackingWriter) answer(req *http.Request, rs *resolver, err error) {
	if t.wrote {
		abort(req, rs, err)
	}
	rs.fail(t, req, err)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Flush sends buffered data, ignoring an error as http.Flusher requires;
// use http.ResponseController to see it.
func (t *trackingWriter) Flush() { _ = t.FlushError() }

// FlushError sends buffered data and reports why it could not. After a
// successful flush the response has started.
func (t *trackingWriter) FlushError() error {
	if err := http.NewResponseController(t.ResponseWriter).Flush(); err != nil {
		return err
	}
	t.wrote = true
	return nil
}

// Hijack takes over the connection; the response is then the handler's, so
// an error it returns afterwards is reported, never written.
func (t *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err == nil {
		t.wrote = true
	}
	return conn, brw, err
}

// abort reports a failure after the response started and cuts the
// connection, so the client sees a failed response rather than a short one
// that ends cleanly.
func abort(req *http.Request, rs *resolver, err error) {
	p, report := rs.resolve(req, err)
	rs.observe(req, fmt.Errorf("%w: %w", ErrEncodeAfterCommit, report), p)
	panic(http.ErrAbortHandler)
}

// contain runs f and returns a panic it raises as a *PanicError, so the
// caller answers it outside the recovery and a panic while answering, such
// as one in OnError, reaches net/http.
func contain(f func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = panicError(v)
		}
	}()
	return f()
}

// panicError turns a recovered value into a *PanicError, re-raising
// http.ErrAbortHandler, which net/http handles itself.
func panicError(v any) error {
	if v == http.ErrAbortHandler {
		panic(v)
	}
	return &PanicError{Value: v, Stack: debug.Stack()}
}
