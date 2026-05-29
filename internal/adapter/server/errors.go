package server

import "errors"

// Sentinel errors the service returns; the gRPC and HTTP adapters map these to
// their respective status codes (codes.InvalidArgument / NotFound, HTTP 400 /
// 404).
var (
	// ErrInvalidArgument signals a malformed or missing required field.
	ErrInvalidArgument = errors.New("server: invalid argument")
	// ErrNotFound signals an unknown session id.
	ErrNotFound = errors.New("server: session not found")
)
