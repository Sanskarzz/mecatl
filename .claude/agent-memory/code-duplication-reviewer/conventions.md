---
name: conventions
description: mecatl duplication conventions — what is deliberately kept separate, what is already centralized, what to skip
metadata:
  type: project
---

mecatl is a from-scratch Go agentic harness (hexagonal: internal/session domain, internal/port interfaces, internal/adapter/* adapters, internal/agent use-case loop). Built in parallel work-packages (WPn), so incidental copies appear across sibling adapters.

**Already centralized correctly (do NOT re-flag as duplication):**
- Snapshot serialization lives in `internal/adapter/store/sessnap`; both `memstore` and `jsonlstore` delegate (`sessnap.Of/Marshal/Unmarshal`). This is correct centralization.
- Tool arg-parsing/truncation/schema/caps shared in `internal/adapter/tools/tools.go` (`parseArgs`, `truncateBytes`, `schema`, max* consts). Per-tool validation/markers are intentionally NOT shared.
- Event Seq-assignment is centralized: `Run.emit` (loop.go) assigns Seq; `Engine.emit` (dispatch.go) wraps it + mirrors to Sink. The proto translation is a single `toProto` family in `server/mapper.go`.

**Deliberately kept separate (do NOT suggest extracting):**
- `osfs` vs `memfs` `cleanPath`: DIFFERENT signatures and semantics (real-FS abs-path join+symlink vs in-memory key normalization). Two FS impls that must stay decoupled. The conformance behavior is shared via the test table, not the impls.

**Genuine finding raised:** the `runWorkspaceConformance` test table is byte-identical in `osfs_test.go` (lines 32-142) and `memfs_test.go` (lines 26-136). SHOULD be shared (e.g. an `fsconformance` package taking a `func(*testing.T) tool.Workspace` factory). Two sites today = Low/Medium; the comments themselves say "SHARED ... an identical copy runs against memfs."

**Finding (recurring): `internal/adapter/memory/tools.go` re-copies the `tools/tools.go` helpers** — `parseArgs` (byte-identical), `schema` (byte-identical), `truncateMemory`+`maxMemoryOutputBytes` (a fork of `truncateBytes`+`maxOutputBytes`, same 25_000 cap). Doc comments even say "mirroring the tools package helper." Two packages today; if a third tool package appears this becomes a clear extraction (a small `toolkit`/`toolutil` package for parseArgs/schema/truncate). The 25_000 cap is one concept duplicated — Medium. Per-tool validation stays separate.

**Leave separate (verified incidental):**
- `agent/fork.go` vs `agent/subagent.go`: child-loop machinery is ALREADY shared — `drainChild` is defined once in subagent.go:257 and reused by fork.go:314. `fireSubagentStop`/`childSessionID` are per-type methods but tiny; the fan-out/join (runBranches, joinBranches, isolation via WorkspaceForker) is genuinely Fork-only. No extraction needed.
- `llmresilience.Wrap` vs `permclassify.Wrap`: superficial rhyme. Different ports (LLMProvider vs PermissionPolicy), different semantics (retry/backoff/breaker vs monotonic risk classify). Only Config+Wrap shape matches — extracting would couple two security/resilience concerns. Leave.
- `server/authn.go`: token-check is already factored to ONE checker — gRPC and HTTP both call `bearerFromAuthValue`+`constantTimeTokenMatch`+`clientKeyFrom*`+`limiterSet`. The auth-then-rate epilogue differs only by transport error type. Correctly DRY.

**Skip:** SPDX/headers, `var _ Iface = (*T)(nil)` compile-time assertions (idiomatic, not dup), generated proto under `contracts/gen`.
