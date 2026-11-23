package problem_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jacoelho/h/problem"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		p     problem.Problem
		valid bool
	}{
		{"client error", problem.Problem{Status: 404}, true},
		{"server error with absolute type", problem.Problem{Status: 503, Type: "https://example.com/probs/down"}, true},
		{"about:blank", problem.Problem{Status: 400, Type: "about:blank"}, true},
		{"extension", problem.Problem{Status: 400, Extra: map[string]any{"balance_2": 30}}, true},
		{"success status", problem.Problem{Status: 200}, false},
		{"status above 599", problem.Problem{Status: 600}, false},
		{"relative type", problem.Problem{Status: 400, Type: "/probs/x"}, false},
		{"reserved extension", problem.Problem{Status: 400, Extra: map[string]any{"status": 1}}, false},
		{"extension starting with a digit", problem.Problem{Status: 400, Extra: map[string]any{"1x": 1}}, false},
		{"extension with a dash", problem.Problem{Status: 400, Extra: map[string]any{"a-b": 1}}, false},
		{"header field", problem.Problem{Status: 401, Header: http.Header{"Www-Authenticate": {"Bearer"}}}, true},
		{"Content-Length header", problem.Problem{Status: 400, Header: http.Header{"Content-Length": {"3"}}}, false},
		{"CORS header", problem.Problem{Status: 400, Header: http.Header{"access-control-allow-origin": {"*"}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate()
			if c.valid && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !c.valid && !errors.Is(err, problem.ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRFC9457Body(t *testing.T) {
	cases := []struct {
		name string
		p    problem.Problem
		want string
	}{
		{
			"blank type takes the reason phrase",
			problem.Problem{Status: 404},
			`{"type":"about:blank","title":"Not Found","status":404}`,
		},
		{
			"RFC 9457 section 3 example",
			problem.Problem{
				Status: 403, Type: "https://example.com/probs/out-of-credit",
				Title: "You do not have enough credit.", Detail: "Your current balance is 30, but that costs 50.",
				Extra: map[string]any{"balance": 30, "accounts": []string{"/account/12345", "/account/67890"}},
			},
			`{"type":"https://example.com/probs/out-of-credit","title":"You do not have enough credit.","status":403,"detail":"Your current balance is 30, but that costs 50.","accounts":["/account/12345","/account/67890"],"balance":30}`,
		},
		{
			"typed problem without title omits it",
			problem.Problem{Status: 409, Type: "https://example.com/probs/conflict"},
			`{"type":"https://example.com/probs/conflict","status":409}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := problem.RFC9457.Write(&buf, c.p); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != c.want+"\n" {
				t.Fatalf("body\n got %s\nwant %s", got, c.want)
			}
		})
	}
	if got := problem.RFC9457.MediaType(); got != "application/problem+json" {
		t.Fatalf("media type %q", got)
	}
}

func TestTextWritesTitle(t *testing.T) {
	var buf bytes.Buffer
	if err := problem.Text.Write(&buf, problem.Problem{Status: 429}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "Too Many Requests\n" {
		t.Fatalf("got %q", got)
	}
}

func TestJSONFormatWritesShape(t *testing.T) {
	f := problem.JSON("application/vnd.legacy+json", func(p problem.Problem) any {
		return struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{p.Status, p.Detail}
	})
	var buf bytes.Buffer
	if err := f.Write(&buf, problem.Problem{Status: 422, Detail: "bad name"}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), `{"code":422,"message":"bad name"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if f.MediaType() != "application/vnd.legacy+json" {
		t.Fatalf("media type %q", f.MediaType())
	}
}

var errNotFound = errors.New("not found")

type limitError struct{ retryAfter int }

func (e *limitError) Error() string { return "limited" }

func TestMappers(t *testing.T) {
	m := problem.Map(
		problem.Mapper{},
		problem.Is(errNotFound, problem.Problem{Status: http.StatusNotFound}),
		problem.As(func(e *limitError) problem.Problem {
			return problem.Problem{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {fmt.Sprint(e.retryAfter)}}}
		}),
	)
	if p, ok := m.Problem(fmt.Errorf("loading: %w", errNotFound)); !ok || p.Status != 404 {
		t.Fatalf("wrapped sentinel: %v %v", p, ok)
	}
	if p, ok := m.Problem(fmt.Errorf("calling: %w", &limitError{retryAfter: 7})); !ok || p.Status != 429 || p.Header.Get("Retry-After") != "7" {
		t.Fatalf("typed error: %v %v", p, ok)
	}
	if _, ok := m.Problem(errors.New("other")); ok {
		t.Fatal("unknown error mapped")
	}
}

func TestMapperErrReportsConstructionMistakes(t *testing.T) {
	cases := []struct {
		name    string
		m       problem.Mapper
		invalid bool
	}{
		{"valid Is", problem.Is(errNotFound, problem.Problem{Status: 404}), false},
		{"Is with a success status", problem.Is(errNotFound, problem.Problem{Status: 200}), true},
		{"As with nil", problem.As[*limitError](nil), true},
		{"Func with nil", problem.Func(nil), true},
		{"Map holding an invalid part", problem.Map(problem.Is(errNotFound, problem.Problem{Status: 404}), problem.Is(errNotFound, problem.Problem{Status: 302})), true},
		{"zero Mapper", problem.Mapper{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Err()
			if c.invalid != (err != nil) {
				t.Fatalf("Err() = %v, want invalid=%v", err, c.invalid)
			}
			if c.invalid && !errors.Is(err, problem.ErrInvalid) {
				t.Fatalf("Err() = %v, want ErrInvalid", err)
			}
		})
	}
	if _, ok := (problem.Mapper{}).Problem(errNotFound); ok {
		t.Fatal("zero Mapper answered")
	}
}

func TestReadRoundTripsRFC9457(t *testing.T) {
	p := problem.Problem{
		Status: 403, Type: "https://example.com/probs/out-of-credit",
		Title: "You do not have enough credit.", Detail: "Your current balance is 30, but that costs 50.",
		Extra: map[string]any{"balance": 30, "accounts": []string{"/account/12345", "/account/67890"}},
	}
	var first bytes.Buffer
	if err := problem.RFC9457.Write(&first, p); err != nil {
		t.Fatal(err)
	}
	got, err := problem.Read(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != p.Status || got.Type != p.Type || got.Title != p.Title || got.Detail != p.Detail || len(got.Extra) != 2 {
		t.Fatalf("read %+v", got)
	}
	var second bytes.Buffer
	if err := problem.RFC9457.Write(&second, got); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("round trip\n first %s\nsecond %s", first.String(), second.String())
	}
}

func TestReadRefusesNonProblems(t *testing.T) {
	for _, body := range []string{``, `[]`, `{"title":"x"}`, `{"status":"404"}`, `{"status":404,"title":7}`} {
		if _, err := problem.Read(strings.NewReader(body)); !errors.Is(err, problem.ErrInvalid) {
			t.Fatalf("Read(%q) = %v, want ErrInvalid", body, err)
		}
	}
}

func TestReadRefusesNullStatus(t *testing.T) {
	if _, err := problem.Read(strings.NewReader(`{"status":null}`)); !errors.Is(err, problem.ErrInvalid) {
		t.Fatalf("Read = %v, want ErrInvalid", err)
	}
}

func TestIsWithNilTargetIsInvalid(t *testing.T) {
	if err := problem.Is(nil, problem.Problem{Status: 404}).Err(); !errors.Is(err, problem.ErrInvalid) {
		t.Fatalf("Err() = %v, want ErrInvalid", err)
	}
}
