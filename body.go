package h

import (
	"errors"
	"io"
	"net/http"
	"os"
	"time"
)

// bodyReader is the request body every route reads through. It bounds the
// size, sets the connection's read deadline from the request context's
// deadline before the first read, and reports failures as *RequestError so
// they are told apart from errors an endpoint produces itself.
//
// A deadline that expires while net/http's own background read runs cancels
// the whole connection's context, so every later request on it sees
// context.Canceled. That read runs once the server's body reached EOF:
// before the handler for a request without a body, or after a middleware
// read the body and replaced it. So the deadline is set only for a request
// with a body and a deadline still ahead, and cleared as soon as this reader
// sees EOF, and never when the server has a ReadTimeout (serverBoundsReads).
// A middleware that consumed the body leaves a window of one
// in-memory read in which the deadline could expire under the background
// read; a deadline that close is already all but spent. After a read error
// the expired deadline is kept: it is what stops net/http's drain of the
// unread body from blocking on a stalled client before the response is
// written.
type bodyReader struct {
	r             io.ReadCloser
	w             http.ResponseWriter
	deadline      time.Time
	deadlineTried bool
	err           error
}

func newBodyReader(w http.ResponseWriter, req *http.Request, limit int64) *bodyReader {
	b := &bodyReader{}
	b.init(w, req, limit)
	return b
}

func (b *bodyReader) init(w http.ResponseWriter, req *http.Request, limit int64) {
	*b = bodyReader{r: http.MaxBytesReader(w, req.Body, limit), w: w}
	if req.ContentLength != 0 && !serverBoundsReads(req) {
		if d, ok := req.Context().Deadline(); ok && time.Until(d) > 0 {
			b.deadline = d
		}
	}
}

// serverBoundsReads reports whether the http.Server set a ReadTimeout. Its
// read deadline was armed when the request began, at a time the handler
// cannot see, so any deadline set here could lift it; the server's bound
// then governs body reads instead of the context's.
func serverBoundsReads(req *http.Request) bool {
	srv, ok := req.Context().Value(http.ServerContextKey).(*http.Server)
	return ok && srv.ReadTimeout > 0
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if !b.deadlineTried && !b.deadline.IsZero() {
		b.deadlineTried = true
		// A writer without deadline support (httptest.ResponseRecorder, some
		// middleware writers without Unwrap) leaves reads bounded only by the
		// server's own ReadTimeout.
		_ = http.NewResponseController(b.w).SetReadDeadline(b.deadline)
	}
	n, err := b.r.Read(p)
	if err == nil {
		return n, nil
	}
	if err != io.EOF {
		err = classifyRead(err)
	}
	b.err = err
	if err == io.EOF && b.deadlineTried {
		_ = http.NewResponseController(b.w).SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *bodyReader) Close() error { return b.r.Close() }

func classifyRead(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &RequestError{Status: http.StatusRequestEntityTooLarge, Err: err}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &RequestError{Status: http.StatusRequestTimeout, Err: err}
	}
	return &RequestError{Status: http.StatusBadRequest, Err: err}
}
