// Package mw holds middleware that composes with h.Router.Use and h.Use, or
// with any http.Handler.
package mw

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"
)

type requestIDKey struct{}

// RequestID returns middleware that takes a request's ID from the header
// field named header, or makes a random one when the field is absent or
// longer than 128 bytes, stores it in the request context and echoes it in
// the response's header field of the same name.
func RequestID(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := req.Header.Get(header)
			if id == "" || len(id) > 128 {
				id = rand.Text()
			}
			w.Header().Set(header, id)
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), requestIDKey{}, id)))
		})
	}
}

// RequestIDFrom returns the request ID RequestID stored in ctx, and whether
// there was one.
func RequestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

// AccessLog returns middleware that logs one line per request to log at
// level: method, route pattern, path, status, bytes written, duration and,
// when RequestID runs before it, the request ID.
func AccessLog(log *slog.Logger, level slog.Leveler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			rw := &recordingWriter{ResponseWriter: w}
			defer func() {
				attrs := []slog.Attr{
					slog.String("method", req.Method),
					slog.String("pattern", req.Pattern),
					slog.String("path", req.URL.Path),
					slog.Int("status", rw.statusOrOK()),
					slog.Int64("bytes", rw.bytes),
					slog.Duration("duration", time.Since(start)),
				}
				if id, ok := RequestIDFrom(req.Context()); ok {
					attrs = append(attrs, slog.String("request_id", id))
				}
				log.LogAttrs(req.Context(), level.Level(), "request", attrs...)
			}()
			next.ServeHTTP(rw, req)
		})
	}
}

type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recordingWriter) WriteHeader(code int) {
	if r.status == 0 && code >= 200 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingWriter) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recordingWriter) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recordingWriter) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Timeout returns middleware that gives each request a context deadline d
// from now. The router bounds request body reads by it, and endpoints see
// it through their context; a request that exceeds it is answered with 503.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithTimeout(req.Context(), d)
			defer cancel()
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}
