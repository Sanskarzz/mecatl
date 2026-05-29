---
name: project-mecatl
description: mecatl Go 1.26 agentic harness — dependency set, governance/security constraints, and which hand-rolled code is intentionally zero-dep
metadata:
  type: project
---

`github.com/stacklok/mecatl` — from-scratch Go 1.26 agentic harness. Owner: mecatl@stacklok.com.

**Direct deps (go.mod, all pass the screen):**
- `github.com/openai/openai-go/v3` v3.37.0 — official OpenAI SDK, Apache-2.0. Has built-in retry (`option.WithMaxRetries`), threaded through via `WithRequestOption` in internal/adapter/openai/openai.go.
- `google.golang.org/grpc` v1.81.x (CNCF), `google.golang.org/protobuf` v1.36.11 (Google/BSD-3) — standard.
- `buf.build/gen/go/.../protovalidate` — Buf-generated protovalidate stubs, Apache-2.0. Appropriate.
- `golang.org/x/time` v0.15.0 — Go team, BSD-3. `rate.Limiter` used correctly in server/authn.go. Right call (no stdlib token bucket).
- `go.opentelemetry.io/otel` + sdk + trace + otlptrace{,grpc,http} v1.44.0 — CNCF, Apache-2.0. Official, foundation-backed. Fine.
- `github.com/prometheus/client_golang` v1.23.2 — CNCF, Apache-2.0. The standard. Fine.
- `github.com/modelcontextprotocol/go-sdk` v1.6.1 — official MCP SDK (Anthropic+Google maintained), MIT. mcp adapter uses StreamableClientTransport / Client / ClientSession — does NOT hand-roll JSON-RPC. Correct.
- Indirect note: `cenkalti/backoff/v5` is pulled in transitively (by otel/grpc), NOT used directly. llmresilience does its own backoff intentionally (see below).

**llmresilience decorator (internal/adapter/llmresilience) — hand-rolled retry/backoff/breaker is JUSTIFIED, do NOT recommend a backoff lib:**
The load-bearing constraint is no-replay-after-first-chunk: it buffers exactly the first streamed chunk (iter.Pull2) and only retries failures before any chunk is observed. A generic backoff lib (cenkalti/backoff) wraps a `func() error` and has no concept of "retryable only until first chunk yielded from an iter.Seq2 stream" — it cannot express this. Backoff math uses full jitter + overflow guard + ctx-aware time.NewTimer (correct). Note: SDK already retries network-level (WithMaxRetries); this layer is a higher-level stream-establishment + circuit-breaker concern, complementary not duplicative. Wired in cmd/mecated/main.go:290 only on the real openai path.

**Hand-rolled code that is INTENTIONALLY zero-dep / security-audited — do NOT recommend a library:**
- `internal/governance/bash.go` SplitCommands + Canonicalize: a deliberately closed, audited shell splitter for the permission boundary. A general shellwords lib (e.g. mattn/go-shellwords, single-author) would be a security regression here — the closed wrapper allowlist and "do not strip re-entrant launchers" logic IS the security property. Leave it.
- SSE decode (internal/adapter/openai/stream.go) and SSE encode (internal/adapter/server/http.go): hand-rolled but trivial and tied to the openai-go event union / proto Event shape. No SSE lib warranted.

**Concurrency:** internal/agent/dispatch.go uses sync.WaitGroup+mutex for a read-only batch with NO error propagation or early cancel (all results captured as values). errgroup would not fit cleanly. Justified.

**Security good-practices already present:** crypto/rand for session IDs (service.go), ReadHeaderTimeout set on http.Server. http.Server is missing ReadTimeout/WriteTimeout/IdleTimeout but WriteTimeout would break the SSE long-poll, so that's expected.

**Go 1.21+ stdlib cleanups available (Low only):** sort.Slice/sort.Strings sites could be slices.SortFunc/slices.Sort (catalog.go:85, prompt/env.go:45, compaction.go:124, evaluator.go:297, memfs.go:153/251). Cosmetic, not findings.
