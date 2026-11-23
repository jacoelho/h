// README begin greet

// Command greet answers GET /greet/{name} with a JSON greeting.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/jacoelho/h"
	"github.com/jacoelho/h/problem"
)

// Greeting is the body GET /greet/{name} answers with.
type Greeting struct {
	Message string `json:"message"`
}

// ErrUnknownName reports a name greet has no greeting for.
var ErrUnknownName = errors.New("unknown name")

func greet(_ context.Context, name string) (Greeting, error) {
	if name == "nobody" {
		return Greeting{}, ErrUnknownName
	}
	return Greeting{Message: "hello, " + name}, nil
}

func main() {
	r, err := h.New(h.Config{
		Errors: problem.Is(ErrUnknownName, problem.Problem{Status: http.StatusNotFound, Title: "Unknown name"}),
	})
	if err != nil {
		log.Fatal(err)
	}
	r.Get("/greet/{name}", h.Bind(func(p *h.Params, _ h.Empty) string {
		return p.Path("name", h.String)
	}, greet))

	handler, err := r.Handler()
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.ListenAndServe("localhost:8080", handler))
}

// README end
