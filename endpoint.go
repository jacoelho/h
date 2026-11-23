package h

import "context"

// Endpoint handles one route: it receives the decoded request and returns
// the response, or an error the router translates into a problem.
type Endpoint[In, Out any] func(context.Context, Request[In]) (Response[Out], error)

// Func lifts a domain function whose input is the whole request body (or
// Empty) into an Endpoint answering 200 with its result, or 204 when Out is
// Empty. A nil f gives a nil Endpoint, which registration reports.
func Func[In, Out any](f func(context.Context, In) (Out, error)) Endpoint[In, Out] {
	if f == nil {
		return nil
	}
	return func(ctx context.Context, r Request[In]) (Response[Out], error) {
		out, err := f(ctx, r.Body())
		if err != nil {
			return Response[Out]{}, err
		}
		return Response[Out]{body: out}, nil
	}
}

// Bind lifts a domain function whose input is built from the request body
// and its parameters. bind reads them through Params; when any is missing or
// malformed the router answers 400 (or 404 for a path value) and f is not
// called. A nil bind or f gives a nil Endpoint, which registration reports.
func Bind[Body, In, Out any](bind func(*Params, Body) In, f func(context.Context, In) (Out, error)) Endpoint[Body, Out] {
	if bind == nil || f == nil {
		return nil
	}
	return func(ctx context.Context, r Request[Body]) (Response[Out], error) {
		var in In
		if err := r.Parse(func(p *Params) { in = bind(p, r.Body()) }); err != nil {
			return Response[Out]{}, err
		}
		out, err := f(ctx, in)
		if err != nil {
			return Response[Out]{}, err
		}
		return Response[Out]{body: out}, nil
	}
}
