//go:build repomap

package main

import (
	"log/slog"

	"github.com/stacklok/ozzharness/internal/adapter/repomap"
	"github.com/stacklok/ozzharness/internal/tool"
)

// registerOptionalTools is the `repomap`-tagged implementation, selected when
// the binary is built WITH `-tags repomap`. It registers the Aider-style
// repo-map tool, which parses source with tree-sitter (a CGO dependency).
//
// Because tree-sitter requires CGO, this file is gated behind the build tag so
// the DEFAULT build (and the CGO-free, static ko image built with
// CGO_ENABLED=0) excludes it. Build with `CGO_ENABLED=1 go build -tags repomap
// ./cmd/ozzd` to include the repo-map tool.
func registerOptionalTools(cat *tool.Catalog) {
	cat.MustRegister(repomap.NewTool())
	slog.Info("repo map tool ENABLED (-tags repomap)")
}
