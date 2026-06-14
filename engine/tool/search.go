package tool

import (
	"context"
	"errors"
)

// ErrSearchUnavailable is the sentinel a SearchProvider returns when no search
// backend is configured (the honest "not-configured" stub). The WebSearch tool
// surfaces it to the model as a tool-level message ("ask the operator to
// configure a search provider") rather than aborting the harness — mirroring the
// ErrNoShell precedent the Bash tool uses for a shell-less CommandRunner.
var ErrSearchUnavailable = errors.New("tool: no search provider configured")

// SearchProvider is the outbound web-search seam the WebSearch tool depends on,
// the way the Bash tool depends on CommandRunner. It lives here in engine/tool,
// NOT engine/port, for the same layering reason FileSystem/Workspace/CommandRunner
// do: it is a TOOL collaborator injected at execution, never a loop port the
// agent.Engine references. The agent loop never names this type — only the
// WebSearch tool does, which is what keeps web search optional in the catalog.
//
// Implementations must be safe for concurrent use: the read-parallel dispatcher
// fans out N concurrent WebSearch calls per turn, so an adapter that performs
// network I/O must carry its OWN per-call timeout AND a concurrency/rate limit
// internally (never relying on the dispatcher or the tool to bound egress).
type SearchProvider interface {
	// Search runs q and returns ranked results, best-first. The Limit on q is
	// already clamped by the tool before the call (the tool is the bounding choke
	// point); an implementation MAY further cap but must never EXCEED it. A
	// not-configured backend returns ErrSearchUnavailable; any other non-nil error
	// is a backend fault the tool renders as a model-facing tool error.
	Search(ctx context.Context, q SearchQuery) ([]SearchResult, error)
}

// SearchQuery is one web-search request. Query is the verbatim user/model query
// — implementations MUST pass it through unchanged (secret-scanning is the
// guardrails layer's job, not the search adapter's). Limit is the clamped result
// cap. Site and Freshness are optional refinements an adapter MAY map onto its
// backend's parameters (or ignore if unsupported).
type SearchQuery struct {
	// Query is the verbatim search string. Never mutated by an adapter.
	Query string
	// Limit is the maximum number of results to return, already clamped by the
	// tool to its default-when-absent and hard-max bounds.
	Limit int
	// Site, when non-empty, restricts results to a single site/domain (e.g.
	// "go.dev"). Adapter maps it onto the backend's site filter if supported.
	Site string
	// Freshness, when non-empty, is an opaque recency hint (e.g. "day", "week",
	// "month") an adapter MAY map onto its backend's recency filter.
	Freshness string
}

// SearchResult is one discovered source. URL is the candidate the model can then
// pass to WebFetch to retrieve the full page; the other fields are compact,
// attribution-oriented metadata. Every field is UNTRUSTED external content — the
// WebSearch tool fences it before it reaches the model.
type SearchResult struct {
	// Title is the result's title/headline.
	Title string
	// URL is the candidate source URL (the handle for a follow-up WebFetch).
	URL string
	// Snippet is a short excerpt/summary of the result.
	Snippet string
	// Date is an optional publication/last-modified date string, as the backend
	// reported it (no parsing/normalisation in the domain).
	Date string
	// Source is an optional source/publisher name (the attribution axis).
	Source string
}
