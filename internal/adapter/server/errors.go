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
	// ErrNoMCPProvider signals that an MCP inspection RPC requiring a live
	// provider (ReadMcpResource / GetMcpPrompt) was called but no MCP provider
	// is configured. Adapters map it to FailedPrecondition / HTTP 412.
	ErrNoMCPProvider = errors.New("server: no MCP provider configured")
	// ErrInternal signals a server-side fault that is NOT the client's fault — a
	// transport/protocol error talking to a downstream (e.g. an MCP server that
	// is connected but errors a read). Adapters map it to Internal / HTTP 500,
	// distinguishing it from a bad request (ErrInvalidArgument). It exists so the
	// MCP read methods can keep an unknown-server name as InvalidArgument while a
	// genuine fault on a known server is reported as a server error.
	ErrInternal = errors.New("server: internal error")
)
