---
name: project-ozzharness
description: ozzharness — a from-scratch headless agentic coding harness in Go; trust boundaries, threat model, and v1 security-boundary design decisions.
metadata:
  type: project
---

ozzharness is a headless agentic coding harness (Go). An LLM drives a streaming
tool loop that executes shell commands, reads/writes files, runs lifecycle
hooks, and is exposed over a gRPC + HTTP/SSE API (`internal/adapter/server`).

**Threat model / trust boundaries:**
- The LLM is the primary untrusted actor. Tool-call arguments (Bash command,
  file paths, edit strings) are attacker-influenced via prompt injection.
- The API (gRPC `Converse` + HTTP REST/SSE) is currently UNAUTHENTICATED — any
  caller who reaches the listen addr can create sessions, start runs, and
  approve permission asks. Workspace root is fully caller-chosen.
- Workspace path-escape boundary: `internal/adapter/osfs` + `memfs` scope all
  paths under a session root.

**v1 boundary design (documented non-goals — do NOT flag as vulns):**
- OS-level sandbox (Landlock/seccomp/bubblewrap) is an explicit v3 item
  (`docs/harnesses/08-design-considerations.md` §"Security levers"). The v1
  boundary is permission-gate + hooks, NOT an OS sandbox.
- Bash runs via `/bin/sh -c` with full user privileges by design.
- stdio MCP is FORBIDDEN by project rule (streaming-HTTP only). Confirmed: no
  `os/exec`-spawned MCP server exists. WebFetch is a no-op stub in v1 (no SSRF
  surface yet).

**Design source of truth:** `docs/harnesses/08-design-considerations.md`
(§"13 load-bearing decisions" #10 permissions, §"Security levers"). The
permission model spec: deny→ask→allow across merged scopes; compound-Bash
aware; process-wrapper canonicalization with a CLOSED list (never strip
`docker exec`/`npx`/`devbox run`/`sudo`).

See [[ref-bash-permission-gate]] for the gate internals and their gaps.
