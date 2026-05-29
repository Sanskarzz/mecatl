// Package mcp adapts tools served by external Model Context Protocol (MCP)
// servers into the harness's tool.Tool interface, so the agent loop can call
// remote MCP tools exactly as it calls built-in ones.
//
// # Transport
//
// This adapter speaks ONLY the MCP Streamable HTTP client transport (HTTP POST
// plus SSE), per the project's hard constraint. The stdio transport is
// deliberately never imported, constructed, or exposed; no MCP server process
// is ever spawned. If the underlying SDK ships a stdio/command transport, this
// package intentionally does not use it.
//
// # SDK
//
// It wraps github.com/modelcontextprotocol/go-sdk/mcp (the official Go SDK),
// using mcp.StreamableClientTransport for the transport and mcp.Client /
// mcp.ClientSession for the handshake, tool listing, and tool invocation.
//
// # Trust
//
// Remote MCP servers are an untrusted supply-chain surface (see
// docs/harnesses/08). Tool names are namespaced as mcp__<server>__<tool> so a
// remote server can never shadow a built-in tool, and the conservative ReadOnly
// default keeps remote tools serialized unless they explicitly advertise a
// read-only hint.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stacklok/ozzharness/internal/tool"
)

// defaultConnectTimeout bounds the initialize handshake and initial tool
// listing so an unresponsive server cannot stall startup indefinitely.
const defaultConnectTimeout = 30 * time.Second

// clientName / clientVersion identify this harness to MCP servers in the
// initialize handshake.
const (
	clientName    = "ozzharness"
	clientVersion = "v0"
)

// ServerConfig describes a single remote MCP server to connect to over the
// Streamable HTTP transport.
type ServerConfig struct {
	// Name is a short, stable identifier for the server. It becomes the
	// <server> segment of every wrapped tool's namespaced name, so it should be
	// unique across the configured servers and contain no "__" sequence.
	Name string
	// URL is the server's Streamable HTTP endpoint (e.g. https://host/mcp).
	URL string
	// Headers are extra HTTP headers sent on every request to the server, such
	// as "Authorization: Bearer ...". Optional.
	Headers map[string]string
	// Timeout bounds the connect handshake and tool listing. If zero,
	// defaultConnectTimeout is used. It does not bound later tool calls, which
	// are governed by the per-call context.
	Timeout time.Duration
}

// headerRoundTripper injects static headers onto every outbound request. It is
// how per-server auth headers reach the Streamable HTTP transport, which only
// exposes an *http.Client seam.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we never mutate a request the caller may reuse.
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// Server is a live connection to one remote MCP server. It owns the SDK client
// session and the tool.Tool wrappers derived from the server's tool list.
type Server struct {
	name    string
	session *mcpsdk.ClientSession
	tools   []tool.Tool
}

// Connect establishes a Streamable HTTP session to the configured MCP server,
// performs the initialize handshake, lists the server's tools, and returns a
// Server whose Tools() are ready to register into a catalog.
//
// The connect handshake and tool listing are bounded by cfg.Timeout (or
// defaultConnectTimeout). A non-nil error means the server should be treated as
// unavailable; callers (the composition root) are expected to log-and-skip such
// a server rather than aborting the whole harness.
func Connect(ctx context.Context, cfg ServerConfig) (*Server, error) {
	if cfg.Name == "" {
		return nil, errors.New("mcp: server config requires a Name")
	}
	if strings.Contains(cfg.Name, "__") {
		return nil, fmt.Errorf("mcp: server name %q must not contain %q", cfg.Name, "__")
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server %q requires a URL", cfg.Name)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpClient := &http.Client{}
	if len(cfg.Headers) > 0 {
		// Copy headers so later mutation of cfg can't affect the live client.
		headers := make(map[string]string, len(cfg.Headers))
		for k, v := range cfg.Headers {
			headers[k] = v
		}
		httpClient.Transport = &headerRoundTripper{
			base:    http.DefaultTransport,
			headers: headers,
		}
	}

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: httpClient,
		// This adapter only issues request/response tool calls; it does not
		// consume server-initiated notifications (e.g. tool-list-changed).
		// Disabling the standalone SSE GET stream avoids holding a persistent
		// connection open, which lets sessions (and test servers) close cleanly.
		DisableStandaloneSSE: true,
	}

	client := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: clientName, Version: clientVersion},
		nil,
	)

	sess, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to server %q: %w", cfg.Name, err)
	}

	tools, err := listTools(connectCtx, cfg.Name, sess)
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("mcp: list tools on server %q: %w", cfg.Name, err)
	}

	return &Server{name: cfg.Name, session: sess, tools: tools}, nil
}

// listTools pages through the server's tools and wraps each as a tool.Tool.
func listTools(ctx context.Context, serverName string, sess *mcpsdk.ClientSession) ([]tool.Tool, error) {
	var tools []tool.Tool
	for remote, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		wrapped, werr := newRemoteTool(serverName, sess, remote)
		if werr != nil {
			return nil, werr
		}
		tools = append(tools, wrapped)
	}
	return tools, nil
}

// Name returns the server's configured name.
func (s *Server) Name() string { return s.name }

// Tools returns the wrapped remote tools exposed by this server.
func (s *Server) Tools() []tool.Tool { return s.tools }

// Close terminates the MCP session. It is safe to call once; subsequent calls
// return the SDK's session-close result.
func (s *Server) Close() error {
	if s.session == nil {
		return nil
	}
	return s.session.Close()
}

// Manager holds a set of connected MCP servers and presents their tools as a
// single aggregate. It is the convenient entry point when wiring several
// servers at once.
type Manager struct {
	servers []*Server
}

// NewManager connects to each ServerConfig over Streamable HTTP. By default a
// server that fails to connect is logged-and-skipped (via onError) so one bad
// server does not take down the harness; the surviving servers are returned in
// the Manager. If onError is nil, connection errors are silently skipped.
//
// NewManager returns an error only if no servers could be connected AND at
// least one was configured, so the caller can distinguish "nothing usable" from
// "all good".
func NewManager(ctx context.Context, configs []ServerConfig, onError func(cfg ServerConfig, err error)) (*Manager, error) {
	m := &Manager{}
	var lastErr error
	var attempted int
	for _, cfg := range configs {
		attempted++
		srv, err := Connect(ctx, cfg)
		if err != nil {
			lastErr = err
			if onError != nil {
				onError(cfg, err)
			}
			continue
		}
		m.servers = append(m.servers, srv)
	}
	if attempted > 0 && len(m.servers) == 0 {
		return m, fmt.Errorf("mcp: no servers could be connected: %w", lastErr)
	}
	return m, nil
}

// Servers returns the successfully connected servers.
func (m *Manager) Servers() []*Server { return m.servers }

// Tools returns the union of every connected server's wrapped tools.
func (m *Manager) Tools() []tool.Tool {
	var all []tool.Tool
	for _, s := range m.servers {
		all = append(all, s.Tools()...)
	}
	return all
}

// Close closes every connected server, returning the first error encountered
// (after attempting to close them all).
func (m *Manager) Close() error {
	var firstErr error
	for _, s := range m.servers {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Register adds each wrapped tool to the catalog. It uses Catalog.Register (not
// MustRegister) so a name collision surfaces as an error rather than a panic; a
// collision should be impossible given the mcp__ namespacing, but two servers
// configured with the same Name (or a server advertising duplicate tools) would
// trip it.
func Register(cat *tool.Catalog, tools []tool.Tool) error {
	for _, t := range tools {
		if err := cat.Register(t); err != nil {
			return fmt.Errorf("mcp: register %q: %w", t.Spec().Name, err)
		}
	}
	return nil
}
