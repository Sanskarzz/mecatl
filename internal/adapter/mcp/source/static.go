package source

import (
	"context"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

// StaticSource is the Source backing the operator-configured --mcp-server
// entries (already parsed into mcp.ServerConfig by the flag). It is the simplest
// possible Source: it just hands back the pre-parsed configs verbatim, with no
// diagnostics and no fatal error. It is the highest-precedence source in the
// conventional resolver so an explicit --mcp-server always wins a name collision
// against a discovered ToolHive workload.
type StaticSource struct {
	// Servers is the pre-parsed config list (empty is fine — yields zero servers).
	Configs []mcp.ServerConfig
}

// Servers returns the static configs unchanged. An empty list is "no servers",
// not an error.
func (s StaticSource) Servers(context.Context) ([]mcp.ServerConfig, []SkipError, error) {
	return s.Configs, nil, nil
}

// Name reports the source label used in diagnostics.
func (StaticSource) Name() string { return "static" }
