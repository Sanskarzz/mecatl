---
name: design-decisions
description: Locked architecture judgment calls for ozzharness v1 (gRPC framework, state model, edit format, event model), plus as-built layering facts.
metadata:
  type: project
---

Judgment calls made in the v1 architecture. Each has a one-line rationale; revisit only with a stated forcing function.

- **gRPC framework: grpc-go + buf + protovalidate** (revised from connect-go; ARCHITECTURE §7). Why: mirrors a downstream consumer house style and its bidi `Converse` pattern — the approval frame returns on the same stream emitting events, no out-of-band correlation. Proto source of truth in `contracts/proto`, generated Go in `contracts/gen/go`. HTTP is a hand-rolled SSE adapter (grpc-gateway cannot map bidi), not connect auto-mapping.
- **Server-side conversation state.** Why: permission "ask" pause/resume and cancellation require the harness to own the loop across requests. SessionStore persists Sessions; client holds only a session_id.
- **Edit format: exact-match SEARCH/REPLACE (old_string/new_string + replace_all).** Why: doc 08 pragmatic default for v1; the three Edit invariants depend on exact match.
- **Event model is domain-owned, not provider-owned.** The loop yields domain `session.Event`; the LLMProvider port yields provider-neutral `port.Chunk` the loop translates. Verified: no OpenAI type escapes `adapter/openai`.
- **Read-only parallel / mutating serial enforced in the loop's tool-dispatch stage** (`agent/dispatch.go`), keyed off `tool.Tool.ReadOnly()`. Verified as-built: maximal RO batches run concurrent, mutating tools run alone.

As-built layering facts (verified by import audit, all green build+vet):
- `agent` imports only domain + `port` (no adapter, no api). Clean.
- `governance` imports NOTHING from internal — fully session-free. `permpolicy` adapter is the anti-corruption seam translating `session.ToolCall`→governance. Correct ACL.
- `port` imports `session`, `prompt`, `tool`, `governance` (return/param types). `FileSystem`/`Workspace` live in `internal/tool` (not `port`) to break a port↔tool cycle — documented at top of `port/doc.go` and `tool/tool.go`.
- `session` imports `governance` (for `ResumeWith(governance.PermissionDecision)` param) — the one domain→domain cross-context edge.
- Shared Engine/Run/Service seam: both gRPC `Converse` and HTTP/SSE wrap one `server.Service` over one `agent.Run`. Not reinvented per surface.

Known weak spots (see findings, not yet fixed as of 2026-05-29):
- `Session.StopReason()` conflates recorded terminal reason with limit-derived computation; `sessnap` round-trips it awkwardly via state-machine replay. WP6 flagged a `Snapshot()/Restore()` accessor pair as the fix.
- Aggregate boundary leaks: `agent/loop.go` and `sessnap` write `sess.Conversation.*` and `sess.Counters` directly, bypassing root methods.

See [[project-ozzharness]] and [[domain-language]].
