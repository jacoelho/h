// Package h serves HTTP routes whose handlers are typed functions,
//
//	func(ctx context.Context, r h.Request[In]) (h.Response[Out], error)
//
// and translates their errors into problem details (RFC 9457) owned by each
// vertical through problem.Mapper. The router reads and decodes request
// bodies within limits, negotiates response media types (RFC 9110), streams
// large responses and sequences, and protects routes against cross-site
// request forgery, with CORS for the same trusted origins.
package h
