package h_test

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/codec"
	"github.com/jacoelho/h/htest"
	"github.com/jacoelho/h/problem"
)

func serve(t *testing.T, r *h.Router) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handlerOf(t, r))
	t.Cleanup(s.Close)
	return s
}

type reply struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
}

func get(t *testing.T, url string, header http.Header) (reply, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, resp.ContentLength}, body, err
}

func blob(n int) h.Endpoint[h.Empty, string] {
	return func(context.Context, h.Request[h.Empty]) (h.Response[string], error) {
		return h.OK(strings.Repeat("x", n)), nil
	}
}

func TestSpillAt(t *testing.T) {
	r := newRouter(t, h.Config{SpillAt: 64 << 10})
	r.Get("/big", blob(4<<20))
	r.Get("/small", blob(1<<10))
	r.Get("/limit", blob(64<<10))
	s := serve(t, r)

	resp, body, err := get(t, s.URL+"/big", http.Header{"Accept": {"text/plain"}})
	if err != nil || len(body) != 4<<20 {
		t.Fatalf("big body %d bytes, %v", len(body), err)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("big response Content-Length %d, want none", resp.ContentLength)
	}
	resp, body, err = get(t, s.URL+"/small", http.Header{"Accept": {"text/plain"}})
	if err != nil || resp.ContentLength != 1<<10 || len(body) != 1<<10 {
		t.Fatalf("small response Content-Length %d, body %d, %v", resp.ContentLength, len(body), err)
	}
	resp, body, err = get(t, s.URL+"/limit", http.Header{"Accept": {"text/plain"}})
	if err != nil || resp.ContentLength != 64<<10 || len(body) != 64<<10 {
		t.Fatalf("response of exactly SpillAt bytes: Content-Length %d, body %d, %v", resp.ContentLength, len(body), err)
	}
}

type brokenAfter struct{ n int }

func (b brokenAfter) Encode(w io.Writer, _ any) error {
	if _, err := w.Write(make([]byte, b.n)); err != nil {
		return err
	}
	return errors.New("encoder broke")
}

func (brokenAfter) CanEncode(reflect.Type) bool { return true }

func TestEncoderFailure(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		aborted bool
	}{
		{"after commit", 100 << 10, true},
		{"before commit", 10 << 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log htest.Errors
			codecs := codec.NewSet()
			if err := codecs.Encodes(brokenAfter{c.n}, "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			r := newRouter(t, h.Config{SpillAt: 64 << 10, Codecs: codecs, OnError: log.OnError})
			r.Get("/x", respond(h.OK(item{"a"})))
			resp, _, err := get(t, serve(t, r).URL+"/x", nil)
			if c.aborted {
				if err == nil {
					t.Fatalf("client read a complete %d response", resp.StatusCode)
				}
				if !errors.Is(log.Last(t).Err, h.ErrEncodeAfterCommit) {
					t.Fatalf("OnError got %v", log.Last(t).Err)
				}
				return
			}
			if err != nil || resp.StatusCode != http.StatusInternalServerError || resp.Header.Get("Content-Type") != "application/problem+json" {
				t.Fatalf("got %d %q, %v", resp.StatusCode, resp.Header.Get("Content-Type"), err)
			}
		})
	}
}

func TestStream(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{OnError: log.OnError})
	r.Get("/file", respond(h.OK(h.NewStream("application/pdf", func(w *h.StreamWriter) error {
		_, err := io.WriteString(w, "%PDF")
		return err
	}))))
	r.Get("/early", respond(h.OK(h.NewStream("application/pdf", func(*h.StreamWriter) error { return errSecret }))))
	r.Get("/late", respond(h.OK(h.NewStream("application/pdf", func(w *h.StreamWriter) error {
		_, _ = io.WriteString(w, "%PDF")
		_ = w.Flush()
		return errSecret
	}))))
	r.Get("/zero", respond(h.OK(h.Stream{})))
	s := serve(t, r)

	resp, body, err := get(t, s.URL+"/file", nil)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/pdf" || string(body) != "%PDF" {
		t.Fatalf("file: %d %q %q %v", resp.StatusCode, resp.Header.Get("Content-Type"), body, err)
	}
	resp, _, _ = get(t, s.URL+"/early", nil)
	if resp.StatusCode != http.StatusInternalServerError || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("early error: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if _, _, err := get(t, s.URL+"/late", nil); err == nil {
		t.Fatal("late error: client read a complete response")
	}
	resp, _, _ = get(t, s.URL+"/zero", nil)
	if resp.StatusCode != http.StatusInternalServerError || !errors.Is(log.Last(t).Err, h.ErrInvalidResponse) {
		t.Fatalf("zero stream: %d, %v", resp.StatusCode, log.Last(t).Err)
	}
}

func ndjsonCodecs(t *testing.T) *codec.Set {
	t.Helper()
	s := codec.Default(codec.JSON{})
	if err := s.Encodes(codec.NDJSON{}, "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	if err := s.Decodes(codec.NDJSON{}, "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFlushBeforeSpillAt(t *testing.T) {
	release := make(chan struct{})
	r := newRouter(t, h.Config{Codecs: ndjsonCodecs(t)})
	r.Get("/feed", func(context.Context, h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(func(yield func(item, error) bool) {
			if !yield(item{"first"}, nil) {
				return
			}
			<-release
			yield(item{"second"}, nil)
		})), nil
	})
	s := serve(t, r)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/feed", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	close(release)
	if err != nil || line != `{"name":"first"}`+"\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
}

func count(n int) iter.Seq2[item, error] {
	return func(yield func(item, error) bool) {
		for i := range n {
			if !yield(item{fmt.Sprint(i)}, nil) {
				return
			}
		}
	}
}

func TestSeqOutput(t *testing.T) {
	r := newRouter(t, h.Config{Codecs: ndjsonCodecs(t)})
	r.Get("/many", func(context.Context, h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(count(10000))), nil
	})
	r.Get("/zero", respond(h.Response[h.Seq[item]]{}))

	w := send(t, r, call{method: "GET", target: "/many", header: http.Header{"Accept": {"application/json"}}})
	var items []item
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil || len(items) != 10000 || items[9999].Name != "9999" {
		t.Fatalf("JSON array: %d items, %v", len(items), err)
	}
	w = send(t, r, call{method: "GET", target: "/many", header: http.Header{"Accept": {"application/x-ndjson"}}})
	lines := strings.Split(strings.TrimSuffix(w.Body.String(), "\n"), "\n")
	if len(lines) != 10000 || lines[1] != `{"name":"1"}` {
		t.Fatalf("NDJSON: %d lines", len(lines))
	}
	w = send(t, r, call{method: "GET", target: "/zero", header: http.Header{"Accept": {"application/json"}}})
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("zero Seq %q", w.Body)
	}
}

func failAt(n int, err error) iter.Seq2[item, error] {
	return func(yield func(item, error) bool) {
		for i := 0; ; i++ {
			if i == n {
				yield(item{}, err)
				return
			}
			if !yield(item{strings.Repeat("x", 1024)}, nil) {
				return
			}
		}
	}
}

type elemEncoderFails struct{}

func (elemEncoderFails) Encode(w io.Writer, v any) error {
	seq, ok := v.(codec.Sequence)
	if !ok {
		return fmt.Errorf("encoding %T, want a codec.Sequence", v)
	}
	for e, err := range seq.Elems() {
		if err != nil {
			return err
		}
		if it, ok := e.(item); !ok || it.Name != "" {
			return errors.New("cannot encode element")
		}
	}
	return nil
}

func (elemEncoderFails) CanEncode(reflect.Type) bool { return true }

type ignoresSourceErrors struct{}

func (ignoresSourceErrors) Encode(w io.Writer, v any) error {
	seq, ok := v.(codec.Sequence)
	if !ok {
		return fmt.Errorf("encoding %T, want a codec.Sequence", v)
	}
	for e, err := range seq.Elems() {
		if err != nil {
			return nil
		}
		it, ok := e.(item)
		if !ok {
			return fmt.Errorf("element %T, want item", e)
		}
		_, _ = io.WriteString(w, it.Name)
	}
	return nil
}

func (ignoresSourceErrors) CanEncode(reflect.Type) bool { return true }

func TestSeqSourceErrors(t *testing.T) {
	cases := []struct {
		name    string
		enc     codec.Encoder
		failAt  int
		status  int
		aborted bool
	}{
		{"source error before commit reaches mappers", codec.JSON{}, 1, 404, false},
		{"encoder failure is internal", elemEncoderFails{}, 1, 500, false},
		{"source error after commit aborts", codec.JSON{}, 200, 0, true},
		{"encoder ignoring source error before commit", ignoresSourceErrors{}, 1, 404, false},
		{"encoder ignoring source error after commit", ignoresSourceErrors{}, 200, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log htest.Errors
			codecs := codec.NewSet()
			if err := codecs.Encodes(c.enc, "application/json"); err != nil {
				t.Fatal(err)
			}
			r := newRouter(t, h.Config{
				Codecs: codecs, SpillAt: 64 << 10, OnError: log.OnError,
				Errors: problem.Is(ErrOrderNotFound, problem.Problem{Status: 404}),
			})
			r.Get("/x", func(context.Context, h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
				return h.OK(h.Items(failAt(c.failAt, ErrOrderNotFound))), nil
			})
			resp, _, err := get(t, serve(t, r).URL+"/x", nil)
			if c.aborted {
				if err == nil {
					t.Fatal("client read a complete response")
				}
				seen := log.Last(t).Err
				if !errors.Is(seen, h.ErrEncodeAfterCommit) || !errors.Is(seen, ErrOrderNotFound) {
					t.Fatalf("OnError got %v", seen)
				}
				return
			}
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
			}
		})
	}
}

func TestClientDisconnectStopsSource(t *testing.T) {
	cleaned := make(chan struct{})
	r := newRouter(t, h.Config{SpillAt: 1})
	r.Get("/endless", func(ctx context.Context, _ h.Request[h.Empty]) (h.Response[h.Seq[item]], error) {
		return h.OK(h.Items(func(yield func(item, error) bool) {
			defer close(cleaned)
			for ctx.Err() == nil {
				if !yield(item{strings.Repeat("x", 512)}, nil) {
					return
				}
			}
		})), nil
	})
	s := serve(t, r)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/endless", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadFull(resp.Body, make([]byte, 4096))
	_ = resp.Body.Close()
	select {
	case <-cleaned:
	case <-time.After(5 * time.Second):
		t.Fatal("source still running 5s after the client left")
	}
}

func TestSeqInput(t *testing.T) {
	var log htest.Errors
	r := newRouter(t, h.Config{MaxBody: 64, OnError: log.OnError})
	var seen int
	var second error
	r.Post("/import", func(_ context.Context, req h.Request[h.Seq[item]]) (h.Response[h.Empty], error) {
		seen = 0
		for _, err := range req.Body().All() {
			if err != nil {
				return h.Response[h.Empty]{}, err
			}
			seen++
		}
		for _, err := range req.Body().All() {
			second = err
		}
		return h.Response[h.Empty]{}, nil
	})
	send(t, r, call{method: "POST", target: "/import", body: `[{"name":"a"},{"name":"b"}]`, header: jsonHeader()}).Expect(t, 204)
	if seen != 2 || !errors.Is(second, h.ErrConsumed) {
		t.Fatalf("seen %d, second pass %v", seen, second)
	}

	send(t, r, call{method: "POST", target: "/import", body: `[{"name":"a"},{"name":"b"},{"name":3}]`, header: jsonHeader()}).Expect(t, 400)
	if seen != 2 {
		t.Fatalf("endpoint saw %d elements before the malformed one, want 2", seen)
	}
	if _, ok := errors.AsType[*codec.DecodeError](log.Last(t).Err); !ok {
		t.Fatalf("OnError got %v", log.Last(t).Err)
	}

	big := "[" + strings.Repeat(`{"name":"a"},`, 10) + `{"name":"a"}]`
	send(t, r, call{method: "POST", target: "/import", body: big, header: jsonHeader()}).Expect(t, 413)
}

func TestRawBody(t *testing.T) {
	var called bool
	r := newRouter(t, h.Config{MaxBody: 8})
	r.Post("/upload", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Empty], error) {
		called = true
		_, err := io.Copy(io.Discard, req.Body().Reader())
		return h.Response[h.Empty]{}, err
	}, h.Consumes("image/*"))

	send(t, r, call{method: "POST", target: "/upload", body: "small", header: http.Header{"Content-Type": {"image/png"}}}).Expect(t, 204)
	send(t, r, call{method: "POST", target: "/upload", body: "far too large", header: http.Header{"Content-Type": {"image/png"}}}).Expect(t, 413)
	called = false
	send(t, r, call{method: "POST", target: "/upload", body: "x", header: http.Header{"Content-Type": {"text/plain"}}}).Expect(t, 415)
	if called {
		t.Fatal("endpoint called for a disallowed media type")
	}
}

func TestJSONMarshalerOption(t *testing.T) {
	codecs := codec.NewSet()
	if err := codecs.Encodes(codec.JSON{Options: []json.Options{json.WithMarshalers(json.MarshalToFunc(func(e *jsontext.Encoder, i item) error {
		return e.WriteToken(jsontext.String("item:" + i.Name))
	}))}}, "application/json"); err != nil {
		t.Fatal(err)
	}
	r := newRouter(t, h.Config{Codecs: codecs})
	r.Get("/x", respond(h.OK(item{"a"})))
	if w := send(t, r, call{method: "GET", target: "/x"}); strings.TrimSpace(w.Body.String()) != `"item:a"` {
		t.Fatalf("body %q", w.Body)
	}
}

func TestStreamWriterOutsideItsStreamReportsClosed(t *testing.T) {
	var zero h.StreamWriter
	if _, err := zero.Write([]byte("x")); !errors.Is(err, h.ErrStreamClosed) {
		t.Fatalf("zero writer Write: %v", err)
	}
	var kept *h.StreamWriter
	s := h.NewStream("text/plain", func(w *h.StreamWriter) error { kept = w; return nil })
	if _, err := s.WriteTo(nil); !errors.Is(err, h.ErrInvalidResponse) {
		t.Fatalf("WriteTo(nil): %v", err)
	}
	r := newRouter(t, h.Config{})
	r.Get("/s", func(context.Context, h.Request[h.Empty]) (h.Response[h.Stream], error) { return h.OK(s), nil })
	send(t, r, call{method: "GET", target: "/s"}).Expect(t, http.StatusOK)
	if _, err := kept.Write([]byte("late")); !errors.Is(err, h.ErrStreamClosed) {
		t.Fatalf("write after the stream returned: %v", err)
	}
	if err := kept.Flush(); !errors.Is(err, h.ErrStreamClosed) {
		t.Fatalf("flush after the stream returned: %v", err)
	}
}

type failingFlusher struct {
	*httptest.ResponseRecorder
}

func (failingFlusher) FlushError() error { return errors.New("flush broke") }

func TestStreamFlushReportsTheConnectionsError(t *testing.T) {
	var got error
	r := newRouter(t, h.Config{})
	r.Get("/s", func(context.Context, h.Request[h.Empty]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/plain", func(w *h.StreamWriter) error {
			got = w.Flush()
			return nil
		})), nil
	})
	handlerOf(t, r).ServeHTTP(failingFlusher{httptest.NewRecorder()}, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/s", http.NoBody))
	if got == nil || got.Error() != "flush broke" {
		t.Fatalf("Flush returned %v, want the connection's error", got)
	}
}

func TestAStreamKeepsItsStatus(t *testing.T) {
	for name, write := range map[string]func(*h.StreamWriter) error{
		"writing nothing": func(*h.StreamWriter) error { return nil },
		"only flushing":   func(w *h.StreamWriter) error { return w.Flush() },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRouter(t, h.Config{})
			r.Get("/s", func(context.Context, h.Request[h.Empty]) (h.Response[h.Stream], error) {
				return h.Status(http.StatusAccepted, h.NewStream("text/plain", write)), nil
			})
			send(t, r, call{method: "GET", target: "/s"}).Expect(t, http.StatusAccepted)
		})
	}
}

func TestStreamWritesNothingOnFailure(t *testing.T) {
	if n, err := (h.Stream{}).WriteTo(io.Discard); n != 0 || !errors.Is(err, h.ErrInvalidResponse) {
		t.Errorf("zero Stream WriteTo = %d, %v; want 0, ErrInvalidResponse", n, err)
	}
	s := h.NewStream("text/plain", func(*h.StreamWriter) error { return nil })
	if n, err := s.WriteTo(nil); n != 0 || !errors.Is(err, h.ErrInvalidResponse) {
		t.Errorf("WriteTo(nil) = %d, %v; want 0, ErrInvalidResponse", n, err)
	}
	var kept *h.StreamWriter
	_, _ = h.NewStream("text/plain", func(w *h.StreamWriter) error { kept = w; return nil }).WriteTo(io.Discard)
	if n, err := kept.Write([]byte("late")); n != 0 || !errors.Is(err, h.ErrStreamClosed) {
		t.Errorf("Write after the stream ended = %d, %v; want 0, ErrStreamClosed", n, err)
	}
}

func TestSeqElemsYieldsTheSourceError(t *testing.T) {
	failed := errors.New("source failed")
	s := h.Items(func(yield func(item, error) bool) { yield(item{}, failed) })
	var got error
	for _, err := range s.Elems() {
		got = err
	}
	if !errors.Is(got, failed) {
		t.Fatalf("Elems yielded %v, want the source error", got)
	}
}

func TestARawBodyRouteAnswersWhileTheClientIsStillSending(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/echo", func(_ context.Context, req h.Request[h.RawBody]) (h.Response[h.Stream], error) {
		return h.OK(h.NewStream("text/plain", func(w *h.StreamWriter) error {
			lines := bufio.NewScanner(req.Body().Reader())
			for lines.Scan() {
				if _, err := w.Write(append(lines.Bytes(), '\n')); err != nil {
					return err
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			return lines.Err()
		})), nil
	}, h.Consumes("text/plain"))
	s := serve(t, r)

	body, send := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/echo", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	t.Cleanup(func() { _ = send.CloseWithError(errors.New("test ended")) })
	go func() {
		<-ctx.Done()
		_ = send.CloseWithError(ctx.Err())
	}()
	go func() { _, _ = io.WriteString(send, "first\n") }()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no response while the body was still open: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	echoed := bufio.NewReader(resp.Body)
	if line, err := echoed.ReadString('\n'); line != "first\n" {
		t.Fatalf("first echo %q, %v", line, err)
	}
	_, _ = io.WriteString(send, "second\n")
	_ = send.Close()
	if line, err := echoed.ReadString('\n'); line != "second\n" {
		t.Fatalf("second echo %q, %v", line, err)
	}
}
