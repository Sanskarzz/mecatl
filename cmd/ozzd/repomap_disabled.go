//go:build !repomap

package main

import "github.com/stacklok/ozzharness/internal/tool"

// registerOptionalTools is the DEFAULT (no-op) implementation, selected when the
// binary is built WITHOUT the `repomap` build tag. It registers nothing, so the
// default build pulls in no CGO dependencies and stays statically linkable
// (CGO_ENABLED=0) — this is the configuration the ko image builds.
//
// To include the tree-sitter-backed repo-map tool, build with `-tags repomap`
// (which requires CGO); see repomap_enabled.go.
func registerOptionalTools(_ *tool.Catalog) {}
