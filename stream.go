package h

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
)

// Seq is a sequence of values read or written one element at a time, so a
// route handles more elements than it could hold in memory. As output, wrap
// a source with Items; the router's codec writes it (a JSON array,
// newline-delimited JSON) while ranging over the source after the endpoint
// returns, so the source owns its resources and must honour its context. As
// input, the codec binds it to the request body and the endpoint ranges over
// All. The zero Seq is empty.
type Seq[T any] struct{ s *seqState[T] }

// seqState is unconsumed until All is first ranged over, consumed after.
// err is the first error the source yielded, kept for the router to answer
// with when an encoder hides it.
type seqState[T any] struct {
	src  iter.Seq2[T, error]
	used bool
	err  error
}

// Items returns a Seq of the elements src yields.
func Items[T any](src iter.Seq2[T, error]) Seq[T] { return Seq[T]{&seqState[T]{src: src}} }

// All returns the elements once; a second call yields ErrConsumed. For a
// sequence read from a request body, malformed input, a body over the limit
// and a read timeout are yielded as errors the router answers with 400, 413
// and 408 when the endpoint returns them.
func (s Seq[T]) All() iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		if s.s == nil {
			return
		}
		var zero T
		if s.s.used {
			yield(zero, ErrConsumed)
			return
		}
		s.s.used = true
		if s.s.src == nil {
			return
		}
		for v, err := range s.s.src {
			if !yield(v, err) {
				return
			}
		}
	}
}

// Elems implements codec.Sequence: it yields the elements as values of type
// any and stops at the first error, which it records.
func (s Seq[T]) Elems() iter.Seq2[any, error] {
	return func(yield func(any, error) bool) {
		for v, err := range s.All() {
			if err != nil {
				if s.s != nil && s.s.err == nil {
					s.s.err = err
				}
				yield(nil, &sourceError{err: err})
				return
			}
			if !yield(v, nil) {
				return
			}
		}
	}
}

// Bind implements codec.SequenceTarget: the elements are read with next as
// the endpoint ranges over All, and a read error ends the sequence.
func (s *Seq[T]) Bind(next func(into any) (bool, error)) {
	*s = Items(func(yield func(T, error) bool) {
		for {
			var v T
			more, err := next(&v)
			if err != nil {
				var zero T
				yield(zero, decodeFault(err))
				return
			}
			if !more || !yield(v, nil) {
				return
			}
		}
	})
}

func (s Seq[T]) sourceErr() error {
	if s.s == nil {
		return nil
	}
	return s.s.err
}

type seqOutput interface{ sourceErr() error }

// RawBody is the input of a route that reads its request body as bytes: an
// upload stored as it arrives, a proxied body. The router checks its
// Content-Type against the route's Consumes and limits its size; errors read
// from it are answered with 400, 408 or 413 when the endpoint returns them.
type RawBody struct {
	contentType string
	r           io.Reader
}

// ContentType returns the request's Content-Type, empty when the client
// sent none. The route's Consumes matches such a body as
// application/octet-stream.
func (b RawBody) ContentType() string { return b.contentType }

// Reader returns the request body. The zero RawBody reads as empty.
func (b RawBody) Reader() io.Reader {
	if b.r == nil {
		return http.NoBody
	}
	return b.r
}

// Stream is the output of a route that writes its response body as bytes in
// a content type of its own: a file download, a proxied response. It skips
// negotiation. An error returned before the first Write is answered as a
// problem; after it, the connection is aborted. A zero Stream is an invalid
// response.
type Stream struct {
	contentType string
	write       func(w *StreamWriter) error
}

// NewStream returns a Stream of contentType written by write.
func NewStream(contentType string, write func(w *StreamWriter) error) Stream {
	return Stream{contentType: contentType, write: write}
}

// ContentType returns the media type the stream is sent as.
func (s Stream) ContentType() string { return s.contentType }

// WriteTo runs the stream's write function against w, outside a router, so
// a test can read what the stream writes. It returns the bytes written and
// the write function's error; a zero Stream writes nothing and returns
// ErrInvalidResponse.
func (s Stream) WriteTo(w io.Writer) (int64, error) {
	if s.write == nil {
		return 0, fmt.Errorf("%w: zero Stream", ErrInvalidResponse)
	}
	if w == nil {
		return 0, fmt.Errorf("%w: WriteTo a nil writer", ErrInvalidResponse)
	}
	sw := &StreamWriter{w: w}
	err := s.write(sw)
	n := sw.n
	*sw = StreamWriter{}
	return n, err
}

// StreamWriter writes a Stream's body. Inside a router, the first Write or
// Flush sends the status and header fields. It is usable only while its
// Stream's write function runs.
type StreamWriter struct {
	w io.Writer
	x *exchange
	n int64
}

// ErrStreamClosed reports a write to a StreamWriter after its Stream's write
// function returned, or to one no Stream made.
var ErrStreamClosed = errors.New("h: stream writer closed")

// Write writes p to the response body.
func (s *StreamWriter) Write(p []byte) (int, error) {
	if s.w == nil {
		return 0, ErrStreamClosed
	}
	s.start()
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
}

// Flush sends buffered data to the client. Outside a router it does
// nothing.
func (s *StreamWriter) Flush() error {
	if s.w == nil {
		return ErrStreamClosed
	}
	if s.x == nil {
		return nil
	}
	s.start()
	return s.x.tw.FlushError()
}

func (s *StreamWriter) start() {
	if s.x != nil && !s.x.tw.wrote {
		s.x.start()
	}
}

// NewRawBody returns a RawBody of contentType reading from r, for calling
// an endpoint in a unit test.
func NewRawBody(contentType string, r io.Reader) RawBody {
	return RawBody{contentType: contentType, r: r}
}
