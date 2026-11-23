// Package serverfield owns which response header fields the router writes
// itself, so that neither an endpoint's response nor a problem sets them.
package serverfield

import "strings"

// Owned reports whether the canonical header field name is one the router
// writes: the body's type and length, and the CORS fields, which only the
// router's policy may answer with.
func Owned(canonical string) bool {
	return canonical == "Content-Type" || canonical == "Content-Length" || strings.HasPrefix(canonical, "Access-Control-")
}
