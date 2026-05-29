---
name: conventions
description: ozzharness duplication conventions — what is deliberately kept separate, what is already centralized, what to skip
metadata:
  type: project
---

ozzharness is a from-scratch Go agentic harness (hexagonal: internal/session domain, internal/port interfaces, internal/adapter/* adapters, internal/agent use-case loop). Built in parallel work-packages (WPn), so incidental copies appear across sibling adapters.

**Already centralized correctly (do NOT re-flag as duplication):**
- Snapshot serialization lives in `internal/adapter/store/sessnap`; both `memstore` and `jsonlstore` delegate (`sessnap.Of/Marshal/Unmarshal`). This is correct centralization.
- Tool arg-parsing/truncation/schema/caps shared in `internal/adapter/tools/tools.go` (`parseArgs`, `truncateBytes`, `schema`, max* consts). Per-tool validation/markers are intentionally NOT shared.
- Event Seq-assignment is centralized: `Run.emit` (loop.go) assigns Seq; `Engine.emit` (dispatch.go) wraps it + mirrors to Sink. The proto translation is a single `toProto` family in `server/mapper.go`.

**Deliberately kept separate (do NOT suggest extracting):**
- `osfs` vs `memfs` `cleanPath`: DIFFERENT signatures and semantics (real-FS abs-path join+symlink vs in-memory key normalization). Two FS impls that must stay decoupled. The conformance behavior is shared via the test table, not the impls.

**Genuine finding raised:** the `runWorkspaceConformance` test table is byte-identical in `osfs_test.go` (lines 32-142) and `memfs_test.go` (lines 26-136). SHOULD be shared (e.g. an `fsconformance` package taking a `func(*testing.T) tool.Workspace` factory). Two sites today = Low/Medium; the comments themselves say "SHARED ... an identical copy runs against memfs."

**Skip:** SPDX/headers, `var _ Iface = (*T)(nil)` compile-time assertions (idiomatic, not dup), generated proto under `contracts/gen`.
