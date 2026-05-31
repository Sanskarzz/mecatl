// Package acp is the Agent Client Protocol (ACP) adapter: it lets an ACP editor
// (Zed, and others that speak ACP) drive the mecatl harness as a subprocess over
// stdio. It is a THIRD wire format alongside gRPC and HTTP/SSE — its own JSON,
// NOT the proto contract — projecting the same domain session.Events onto the
// ACP session/update surface.
//
// ACP is JSON-RPC 2.0 over stdio: the editor spawns the agent and the two speak
// over the agent's stdin/stdout. The adapter is therefore BOTH a JSON-RPC server
// (it handles inbound initialize / session/new / session/prompt requests and the
// session/cancel notification) AND a JSON-RPC client (it issues the outbound
// session/request_permission request and correlates the reply, and it pushes
// session/update notifications). The bidirectional codec lives in conn.go.
//
// Layering: this is an ADAPTER. It consumes the surface-agnostic
// *server.Service (CreateSession / StartRun / Approve / Cancel) and the domain
// session types, exactly as the gRPC/HTTP adapters do. It is wired only in the
// composition root (cmd/mecated, behind --acp). It never imports contracts/gen:
// ACP carries its own JSON, decoupled from the proto.
//
// SCOPE — this is Phase 1 (the core loop). The following are DEFERRED to later
// phases and documented in docs/adr/0001-acp-adapter.md:
//   - fs/* delegation (readTextFile/writeTextFile) — we advertise both false and
//     use mecatl's own osfs workspace rooted at the session cwd.
//   - allow_always behaves as allow_once (no rule persistence yet).
//   - diff content blocks for edits (we send plain text tool_call_update content).
//   - session/load (loadSession:false), session modes/config, slash commands.
//   - full-fidelity projection of turn.*/hook/compaction/subagent.*/team.* events
//     (these are dropped or folded into a thought/message chunk this phase).
package acp
