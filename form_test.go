package h_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/htest"
)

type signup struct {
	Name   string
	Age    int64
	Tags   []string
	Avatar string
	Query  string
}

func signupEndpoint(got *signup) h.Endpoint[h.Form, h.Empty] {
	return func(_ context.Context, req h.Request[h.Form]) (h.Response[h.Empty], error) {
		err := req.Body().Parse(func(p *h.FormParams) {
			got.Name = p.Value("name", h.String)
			got.Age = p.ValueOr("age", 18, h.Int64)
			got.Tags = p.Values("tag", h.String)
			got.Query, _ = p.ValueOpt("q", h.String)
			if f, ok := p.FileOpt("avatar"); ok {
				rc, err := f.Open()
				if err == nil {
					b, _ := io.ReadAll(rc)
					_ = rc.Close()
					got.Avatar = string(b)
				}
			}
		})
		return h.Response[h.Empty]{}, err
	}
}

func multipartBody(t *testing.T, fields map[string]string, files map[string]string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range files {
		fw, err := mw.CreateFormFile(k, k+".bin")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(fw, v)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

func TestURLEncodedForm(t *testing.T) {
	for _, method := range []string{"POST", "PUT"} {
		t.Run(method, func(t *testing.T) {
			var got signup
			r := newRouter(t, h.Config{})
			r.Post("/signup", signupEndpoint(&got))
			r.Put("/signup", signupEndpoint(&got))
			w := send(t, r, call{method: method, target: "/signup?q=from-query", body: "name=ana&tag=a&tag=b",
				header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
			w.Expect(t, http.StatusNoContent)
			want := signup{Name: "ana", Age: 18, Tags: []string{"a", "b"}}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("got %+v, want %+v (query values must not leak into the form)", got, want)
			}
		})
	}
}

func TestMultipartForm(t *testing.T) {
	var got signup
	r := newRouter(t, h.Config{})
	r.Post("/signup", signupEndpoint(&got))
	body, ct := multipartBody(t, map[string]string{"name": "bo", "age": "40", "q": "search"}, map[string]string{"avatar": "PNGDATA"})
	w := send(t, r, call{method: "POST", target: "/signup", body: body, header: http.Header{"Content-Type": {ct}}})
	w.Expect(t, http.StatusNoContent)
	if got.Name != "bo" || got.Age != 40 || got.Avatar != "PNGDATA" || got.Query != "search" {
		t.Fatalf("got %+v", got)
	}
}

func TestFormRefusals(t *testing.T) {
	r := newRouter(t, h.Config{})
	var got signup
	r.Post("/signup", signupEndpoint(&got))
	send(t, r, call{method: "POST", target: "/signup", body: `{"name":"a"}`, header: jsonHeader()}).Expect(t, http.StatusUnsupportedMediaType)
	w := send(t, r, call{method: "POST", target: "/signup", body: "age=old",
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
	w.Expect(t, http.StatusBadRequest)
	wantJSON(t, w.Body.Bytes(), `{"type":"about:blank","title":"Bad Request","status":400,"invalid_params":[
		{"in":"form","name":"name","reason":"required"},{"in":"form","name":"age","reason":"invalid"}]}`)
}

func TestMissingFileIsUsableHeader(t *testing.T) {
	r := newRouter(t, h.Config{})
	r.Post("/upload", func(_ context.Context, req h.Request[h.Form]) (h.Response[h.Empty], error) {
		var name string
		var present bool
		err := req.Body().Parse(func(p *h.FormParams) {
			_, present = p.FileOpt("doc")
			name = p.File("doc").Filename
		})
		if name != "" || present {
			t.Errorf("missing file named %q, reported present %v", name, present)
		}
		return h.Response[h.Empty]{}, err
	})
	body, ct := multipartBody(t, map[string]string{"x": "1"}, nil)
	w := send(t, r, call{method: "POST", target: "/upload", body: body, header: http.Header{"Content-Type": {ct}}})
	w.Expect(t, http.StatusBadRequest)
	wantJSON(t, w.Body.Bytes(), `{"type":"about:blank","title":"Bad Request","status":400,"invalid_params":[{"in":"file","name":"doc","reason":"required"}]}`)
}

func TestMultipartTempFilesRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	r := newRouter(t, h.Config{MaxMemory: 1, MaxBody: 1 << 20})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), markerKey{}, 1)))
		})
	})
	var during int
	r.Post("/upload", func(_ context.Context, req h.Request[h.Form]) (h.Response[h.Empty], error) {
		entries, _ := os.ReadDir(dir)
		during = len(entries)
		return h.Response[h.Empty]{}, req.Body().Parse(func(p *h.FormParams) { p.File("doc") })
	})
	body, ct := multipartBody(t, nil, map[string]string{"doc": strings.Repeat("x", 64<<10)})
	send(t, r, call{method: "POST", target: "/upload", body: body, header: http.Header{"Content-Type": {ct}}}).Expect(t, http.StatusNoContent)
	if during == 0 {
		t.Fatal("no temporary file while the endpoint ran; the test does not exercise cleanup")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestTooManyMultipartParts(t *testing.T) {
	fields := map[string]string{}
	for i := range 1100 {
		fields[fmt.Sprint("f", i)] = "v"
	}
	body, ct := multipartBody(t, fields, nil)
	r := newRouter(t, h.Config{MaxBody: 1 << 20})
	var got signup
	r.Post("/signup", signupEndpoint(&got))
	req := httptest.NewRequest("POST", "/signup", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	htest.New(t, r).Send(req).Expect(t, http.StatusRequestEntityTooLarge)
}

func TestASmallUploadStaysInMemoryByDefault(t *testing.T) {
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	r := newRouter(t, h.Config{})
	var onDisk []os.DirEntry
	var readErr error
	r.Post("/upload", func(_ context.Context, req h.Request[h.Form]) (h.Response[h.Empty], error) {
		err := req.Body().Parse(func(p *h.FormParams) { p.File("doc") })
		onDisk, readErr = os.ReadDir(tmp)
		return h.Response[h.Empty]{}, err
	})
	body, ct := multipartBody(t, nil, map[string]string{"doc": "small"})
	send(t, r, call{method: "POST", target: "/upload", body: body, header: http.Header{"Content-Type": {ct}}}).Expect(t, http.StatusNoContent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(onDisk) != 0 {
		t.Fatalf("a 5-byte upload under the 1 MiB default MaxMemory went to disk: %v", onDisk)
	}
}

func TestAFormContentTypeWithAMalformedParameterIsStillAForm(t *testing.T) {
	var got signup
	r := newRouter(t, h.Config{})
	r.Post("/signup", signupEndpoint(&got))
	send(t, r, call{method: "POST", target: "/signup", body: "name=ana",
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset"}}}).Expect(t, http.StatusNoContent)
	if got.Name != "ana" {
		t.Fatalf("got %+v", got)
	}
}

func TestAMultipartContentTypeWithAMalformedParameterIsRefusedAs400(t *testing.T) {
	r := newRouter(t, h.Config{})
	var got signup
	r.Post("/signup", signupEndpoint(&got))
	body, ct := multipartBody(t, map[string]string{"name": "bo"}, nil)
	send(t, r, call{method: "POST", target: "/signup", body: body,
		header: http.Header{"Content-Type": {ct + "; charset"}}}).Expect(t, http.StatusBadRequest)
}

func TestAFormContentTypeWithADuplicateParameterIsStillAForm(t *testing.T) {
	var got signup
	r := newRouter(t, h.Config{})
	r.Post("/signup", signupEndpoint(&got))
	send(t, r, call{method: "POST", target: "/signup", body: "name=ana",
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=utf-8; charset=latin1"}}}).Expect(t, http.StatusNoContent)
	if got.Name != "ana" {
		t.Fatalf("got %+v", got)
	}
}

func TestAMultipartContentTypeWithADuplicateBoundaryIsRefusedAs400(t *testing.T) {
	r := newRouter(t, h.Config{})
	var got signup
	r.Post("/signup", signupEndpoint(&got))
	body, ct := multipartBody(t, map[string]string{"name": "bo"}, nil)
	send(t, r, call{method: "POST", target: "/signup", body: body,
		header: http.Header{"Content-Type": {ct + "; boundary=other"}}}).Expect(t, http.StatusBadRequest)
}
