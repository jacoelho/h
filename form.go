package h

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
)

// Form is the input of a route that reads an HTML form body:
// application/x-www-form-urlencoded or multipart/form-data. The router
// parses it within the route's limits and removes multipart temporary files
// when the request ends. Form values come only from the body, never from
// the query string.
type Form struct {
	st     *requestState
	values url.Values
	files  map[string][]*multipart.FileHeader
}

// NewFormRequest returns a Request carrying a form with values and files,
// for calling an endpoint in a unit test. Path, query and header values
// come from r; a nil r stands for an empty GET / request.
func NewFormRequest(r *http.Request, values url.Values, files map[string][]*multipart.FileHeader) Request[Form] {
	var st *requestState
	if r != nil {
		st = &requestState{req: r}
	}
	return Request[Form]{st: st, body: Form{st: st, values: values, files: files}}
}

// Parse reads form values and files, as well as path, query and header
// values, through fn, with the same error reporting as Request.Parse.
func (f Form) Parse(fn func(*FormParams)) error {
	fp := &FormParams{Params: &Params{st: stateOf(f.st)}, form: f}
	fn(fp)
	return fp.err()
}

// FormParams reads form values and files inside Form.Parse. It also reads
// path, query and header values through its Params.
type FormParams struct {
	*Params
	form Form
}

// Value returns the first value of the form field name parsed by parse,
// recording ErrMissing when it is absent.
func (p *FormParams) Value[T any](name string, parse func(string) (T, error)) T {
	return required(p.Params, "form", name, p.form.values[name], parse)
}

// ValueOr returns the first value of the form field name parsed by parse,
// or def when it is absent.
func (p *FormParams) ValueOr[T any](name string, def T, parse func(string) (T, error)) T {
	return orDefault(p.Params, "form", name, p.form.values[name], def, parse)
}

// ValueOpt returns the first value of the form field name parsed by parse,
// and whether it was present.
func (p *FormParams) ValueOpt[T any](name string, parse func(string) (T, error)) (T, bool) {
	return first(p.Params, "form", name, p.form.values[name], parse)
}

// Values returns every value of the form field name, in order, each parsed
// by parse.
func (p *FormParams) Values[T any](name string, parse func(string) (T, error)) []T {
	return parseEach(p.Params, "form", name, p.form.values[name], parse)
}

// File returns the first file of the multipart field name, recording
// ErrMissing when there is none. On failure it returns an empty, non-nil
// header, so reading it before checking the error does not panic.
func (p *FormParams) File(name string) *multipart.FileHeader {
	fs := p.form.files[name]
	if len(fs) == 0 {
		p.fail("file", name, ErrMissing)
		return &multipart.FileHeader{}
	}
	return fs[0]
}

// FileOpt returns the first file of the multipart field name, and whether
// there was one.
func (p *FormParams) FileOpt(name string) (*multipart.FileHeader, bool) {
	fs := p.form.files[name]
	if len(fs) == 0 {
		return &multipart.FileHeader{}, false
	}
	return fs[0], true
}

// Files returns every file of the multipart field name.
func (p *FormParams) Files(name string) []*multipart.FileHeader { return p.form.files[name] }

// parseForm reads the form body. cleanup is non-nil when temporary files may
// exist and must be called when the request ends.
func parseForm(st *requestState, body io.Reader, maxMemory int64) (f Form, cleanup func() error, err error) {
	mt, params, err := mime.ParseMediaType(st.req.Header.Get("Content-Type"))
	if err != nil {
		return Form{}, nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
	}
	switch mt {
	case "application/x-www-form-urlencoded":
		b, err := io.ReadAll(body)
		if err != nil {
			return Form{}, nil, requestFault(err, http.StatusBadRequest)
		}
		vs, err := url.ParseQuery(string(b))
		if err != nil {
			return Form{}, nil, &RequestError{Status: http.StatusBadRequest, Err: err}
		}
		return Form{st: st, values: vs}, nil, nil
	case "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return Form{}, nil, &RequestError{Status: http.StatusBadRequest, Err: errors.New("multipart body without a boundary")}
		}
		mf, err := multipart.NewReader(body, boundary).ReadForm(maxMemory)
		if err != nil {
			if errors.Is(err, multipart.ErrMessageTooLarge) {
				return Form{}, nil, requestFault(err, http.StatusRequestEntityTooLarge)
			}
			return Form{}, nil, requestFault(err, http.StatusBadRequest)
		}
		return Form{st: st, values: mf.Value, files: mf.File}, mf.RemoveAll, nil
	}
	return Form{}, nil, &RequestError{Status: http.StatusUnsupportedMediaType, Err: ErrUnsupportedMediaType}
}
