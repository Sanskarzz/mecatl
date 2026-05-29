---
name: project-ozzharness
description: ozzharness Go 1.26 agentic harness — dependency set, governance/security constraints, and which hand-rolled code is intentionally zero-dep
metadata:
  type: project
---

`github.com/stacklok/ozzharness` — from-scratch Go 1.26 agentic harness. Owner: ozz@stacklok.com.

**Direct deps (go.mod, all pass the screen):**
- `github.com/openai/openai-go/v3` v3.37.0 — official OpenAI SDK, Apache-2.0. Has built-in retry (`option.WithMaxRetries`), threaded through via `WithRequestOption` in internal/adapter/openai/openai.go. No hand-rolled retry exists anywhere — good.
- `google.golang.org/grpc` v1.81.0 (CNCF), `google.golang.org/protobuf` v1.36.11 (Google/BSD-3) — standard.
- `buf.build/gen/go/.../protovalidate` — Buf-generated protovalidate stubs, Apache-2.0. Appropriate.

**Hand-rolled code that is INTENTIONALLY zero-dep / security-audited — do NOT recommend a library:**
- `internal/governance/bash.go` SplitCommands + Canonicalize: a deliberately closed, audited shell splitter for the permission boundary. A general shellwords lib (e.g. mattn/go-shellwords, single-author) would be a security regression here — the closed wrapper allowlist and "do not strip re-entrant launchers" logic IS the security property. Leave it.
- SSE decode (internal/adapter/openai/stream.go) and SSE encode (internal/adapter/server/http.go): hand-rolled but trivial and tied to the openai-go event union / proto Event shape. No SSE lib warranted.

**Concurrency:** internal/agent/dispatch.go uses sync.WaitGroup+mutex for a read-only batch with NO error propagation or early cancel (all results captured as values). errgroup would not fit cleanly. Justified.

**Security good-practices already present:** crypto/rand for session IDs (service.go), ReadHeaderTimeout set on http.Server. http.Server is missing ReadTimeout/WriteTimeout/IdleTimeout but WriteTimeout would break the SSE long-poll, so that's expected.

**Go 1.21+ stdlib cleanups available (Low only):** sort.Slice/sort.Strings sites could be slices.SortFunc/slices.Sort (catalog.go:85, prompt/env.go:45, compaction.go:124, evaluator.go:297, memfs.go:153/251). Cosmetic, not findings.
