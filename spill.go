package h

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"sync"
)

// spillBuffers holds buffers of at most maxPooledSpill bytes, so a router
// under load reuses them instead of allocating one per response.
var spillBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const maxPooledSpill = 256 << 10

// spillWriter buffers a response body up to limit bytes, so a small body is
// sent with Content-Length and a failure while encoding it can still become
// a problem. Past the limit, or on Flush, it sends the status and streams
// the rest.
type spillWriter struct {
	x     *exchange
	limit int
	buf   *bytes.Buffer
}

func (s *spillWriter) Write(p []byte) (int, error) {
	if s.x.tw.wrote {
		return s.x.tw.Write(p)
	}
	if s.buf == nil {
		s.buf = spillBuffers.Get().(*bytes.Buffer)
	}
	s.buf.Write(p)
	if s.buf.Len() > s.limit {
		if err := s.commit(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush sends the status and everything written so far. A connection that
// cannot flush is not an error here: codecs flush when they can, and the
// bytes still reach the client when the handler returns.
func (s *spillWriter) Flush() error {
	if err := s.commit(); err != nil {
		return err
	}
	if err := s.x.tw.FlushError(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (s *spillWriter) commit() error {
	if s.x.tw.wrote {
		return nil
	}
	s.x.start()
	if s.buf == nil {
		return nil
	}
	_, err := s.x.tw.Write(s.buf.Bytes())
	s.release()
	return err
}

// finish sends a body that never passed the limit, with its length.
func (s *spillWriter) finish() error {
	if s.x.tw.wrote {
		return nil
	}
	n := 0
	if s.buf != nil {
		n = s.buf.Len()
	}
	s.x.tw.Header().Set("Content-Length", strconv.Itoa(n))
	return s.commit()
}

func (s *spillWriter) release() {
	if s.buf == nil {
		return
	}
	if s.buf.Cap() <= maxPooledSpill {
		s.buf.Reset()
		spillBuffers.Put(s.buf)
	}
	s.buf = nil
}
