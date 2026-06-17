# Providers — OpenAI adapter & multi-provider

> Part of the [mecatl architecture guide](../architecture.md).

## The OpenAI Responses adapter (`internal/adapter/openai`)

> This section walks one adapter end-to-end. It is **not** the whole LLM story:
> mecatl is provider-agnostic, with the native Anthropic Messages API as a peer
> adapter and a per-session provider/model registry — see the [multi-provider](#multi-provider--registry-per-session-routing--model-inventory) section below.
> The deeper design brief for this adapter is
> [`docs/adr/0017-openai-responses-api.md`](../adr/0017-openai-responses-api.md).

`Provider` implements `port.LLMProvider` over `POST /v1/responses` using
`github.com/openai/openai-go/v3`. It owns its own conversation state
("strategy B"): every request is **stateless** — `Store: false`, no
`previous_response_id` — and resends the full input item slice.

**Request translation** (`request.go`, `buildParams`):
- `LLMRequest.System` (the `prompt.Layered`) is `Render()`-ed into
  `Instructions`.
- `Tools` → function tools, each spec's JSON `Schema` unmarshalled into the
  SDK's parameter map (`Strict: true`; empty schema → empty object).
- `Messages` → the input item array via `buildInput`: system/user/assistant text
  become message items; tool messages become `function_call_output` items keyed
  by `call_id`. An assistant turn expands (`assistantItems`) in the order
  **reasoning item → function_call item(s) → text message**.
- `Store: false` plus `Include: [reasoning.encrypted_content]` so reasoning
  survives across turns statelessly. `Message.Reasoning` is carried verbatim as
  the reasoning item's `EncryptedContent`.

**SSE → Chunk translation** (`stream.go`, `translate` — a pure function driven
directly from recorded fixtures by `decodeSSE` in tests):
- `response.output_text.delta` → `ChunkText`
- `response.reasoning_summary_text.delta` / `response.reasoning_text.delta` →
  `ChunkReasoning` (the DISPLAY summary)
- `response.output_item.done` (reasoning) → `ChunkReasoningItem` (the
  `encrypted_content` REPLAY blob — distinct from the display summary; the two
  must never be conflated)
- `response.output_item.done` (message) → `ChunkPhase` (the opaque phase
  marker, stored on `Message.ProviderPhase` and replayed verbatim on the
  assistant message item — issue #46)
- `response.output_item.done` (function_call) → `ChunkToolCall` (acts on the
  assembled `.done` payload, not concatenated deltas)
- `response.completed` → `ChunkUsage` then `ChunkDone(end_turn)` (cached tokens
  map into `Usage.CacheReadTokens`)
- `response.incomplete` → `ChunkUsage` then `ChunkDone(error)`
- `response.failed` / `error` → a non-nil stream **error** carrying the
  provider's message verbatim (so the real reason reaches the terminal
  `result`, not an opaque "error")

**Cancellation**: `Stream` (`openai.go`) selects on `ctx.Done()` each iteration
and abandons the underlying stream; a deliberate `ctx` cancel is **not** reported
as a stream error.

**The provider-neutral seam**: the loop only ever sees `port.Chunk`; no OpenAI
type crosses the boundary. The fake `mockllm.Provider` (`engine/adapter/mockllm`,
`New(turns...)`, `TextTurn`) implements the same port for offline loop testing.

**Compatible endpoints**: `WithBaseURL(url)` overrides the host (vLLM, LiteLLM,
a local proxy); the SDK appends `/responses`. `WithAPIKey` and
`WithRequestOption` round out the options. `cmd/mecated` plumbs
`--openai-base-url` through to it.

## Multi-provider — registry, per-session routing & model inventory

mecatl can serve more than one LLM provider in one process and bind a **provider +
model per session**. The wiring lives entirely in the composition layer
(`internal/app`); the domain/agent/server never see a registry — they receive a bare
`port.LLMProvider`.

**The registry (`internal/app/registry.go`).** `buildProviderRegistry` constructs,
once at `Build`, the set of AVAILABLE providers — a provider is available iff one of
its credential env vars resolves (the var NAMES come from the embedded models.dev
catalog, `internal/adapter/providercatalog`; `OPENAI_API_KEY`/`OPENROUTER_API_KEY`/`ANTHROPIC_API_KEY`).
Only available providers are held (an unkeyed provider is omitted — its availability
is itself sensitive, CWE-200). OpenRouter rides the SAME stateless openai adapter with
the OpenRouter base URL substituted. **Anthropic (P1) is the first native non-OpenAI
wire adapter** (`internal/adapter/anthropic`, on the official MIT `anthropic-sdk-go`):
the native Messages API, also STATELESS full-replay, wired via `newAnthropicEntry`. It
validated the provider abstraction — it shipped with NO domain/agent/server/acp/proto
edit; `engineDepsForProvider`, per-session routing, the capability intersection, and
per-sub-agent-provider switching all treat it as data. Its wire-divergences (the
REQUIRED `max_tokens`, the model-class-dependent extended-thinking config which is ON
and model-aware, and the `(thinking,signature[],redacted)` reasoning-replay list packed
into the opaque `Message.Reasoning` STRING) are absorbed at adapter-construction, not in
the DTO. `UseMock` short-circuits to a single synthetic
`mock` entry (offline). The zero-keys case is the named, actionable `errNoProvider`.
`buildProvider` returns the registry **and** its default provider so the shared engine
+ every child/fork/team engine keep receiving the single default provider exactly as
before (the default path is byte-identical). A composition-only `providerConstructor`
seam (mirroring `envDetector`) lets the offline e2e back two real provider ids with
mocks; production leaves it nil.

**Per-session routing (`sessionEngineFactory`).** `CreateSession` carries an OPTIONAL
`provider_id`/`model_id` selector, expressed at the server boundary as the NEUTRAL
`server.ProviderSelector` (the server adapter imports neither the registry nor the
catalog). The widened `SessionEngineFactory func(ctx, sel, specs)` is the ONE seam for
a per-session engine — it serves BOTH a non-default provider/model AND client-provided
MCP servers (orthogonal inputs → ONE engine over ONE catalog). The composition factory
resolves the selector against the registry and builds Deps via
**`engineDepsForProvider`**, which re-derives EVERY provider/model-closing field
(LLM, Compactor, Model, model-keyed TokenCounter, `PromptConfig.Env.Model`, and the
**context-window resolver** `Deps.ContextWindow` — a `func() int` built by
`reg.windowResolver` (override→live→catalog→128k floor) and read live at the point of
use, so the compaction trigger AGREES with the `ListModels`-advertised `context_limit`
and self-corrects after a live-catalog swap with no rebuild; only a genuinely
uncatalogued passthrough model falls back to the 128k default). The DEFAULT model
resolves through the SAME resolver (`baseEngineDeps`, issue #63) — it is no longer
pinned to the 128k floor.
This is the contamination fix: a shallow clone swapping only the
LLM would compact/count through the wrong model. The resolution table:

| `provider_id` | `model_id` | Outcome |
|---|---|---|
| `""` | `""` | **Shared engine** (default provider, no per-session build) — today's path. The default itself resolves `--model` → the server-configured deployment default (`--default-provider`/`--default-model`, issue #21; validated **fail-fast** at build) → the per-provider built-in |
| `""` | set | **InvalidArgument** — a bare model on the env-derived default provider is ambiguous |
| known+available | `""` | per-session engine on that provider's default model |
| known+available | catalogued | per-session engine bound to (provider, model) |
| known+available | NOT catalogued | **passthrough** — the model string reaches the provider verbatim (catalog gates nothing) |
| unknown/unavailable | any | **InvalidArgument** — `"unknown or unavailable provider"`, never a silent fallback |

The provider is **fixed for the session lifetime** (reasoning-replay + the byte-stable
prefix are provider-private; "switch provider" = new session). `session.Session` is
NOT widened — the selector resolves to an ENGINE at create time, registered in the same
`sessionEngines` map (and selected the same way by `StartRunContent`) the client-MCP
path uses; `loadAndReopen` is untouched. That map is **capped** at
`Config.MaxSessionEngines` (default 1024): the gRPC/HTTP surfaces have no
connection-teardown drain, so without a cap a client creating selector sessions and
never calling `CloseSession`/`EndSession` could grow it unbounded (CWE-770). Past the
cap, `createSession` returns `ErrTooManySessionEngines` (gRPC `ResourceExhausted` /
HTTP 429); `CloseSession`/`EndSession` frees a slot. (Keys are NEVER on the wire — only
the provider id.)

**Model inventory (`ListModels` / `internal/app/modelsnapshot.go`).** `modelSnapshot`
joins the registry's AVAILABLE providers to the embedded catalog and projects each
model into the proto `ModelInfo` (public metadata only — id, provider_id, display_name,
image/reasoning flags, context_limit — never a key/env/base-URL). The composition root
injects the snapshot into `server.Config.Models`; the server adapter holds only the
proto slice (mirroring the `ListAgents` idiom). The `mock` provider advertises no
selectable models. `ServerCapabilities.model_selection` is true iff the snapshot is
non-empty, gating the client's model picker the way `agents` gates `/agents`. Provider
key/base-URL flags landed in `cmd/mecated` earlier; the picker UX is a client concern.

**Capability single-source (`internal/app/capability.go`).** A model's true input
capability is the INTERSECTION `catalog-per-model-modalities ∩ adapter-Capabilities()`,
computed by `modelCapability` in composition (the only layer holding both inputs). That
ONE neutral `port.ProviderCapabilities` feeds three sinks so they cannot disagree:
`ModelInfo.image` (ListModels), the `CreateSessionResponse.session_capabilities` echo
(per-session), and the ACP gate (`Service.ProviderCapabilities()`, the default caps).
The server/acp adapters receive only the computed value — no catalog/registry type
crosses inward. Keys are never on the wire — only the provider id.

**Per-sub-agent provider (shipped).** A Subagent agent def or team member may pin a
`provider:` (orthogonal to `model:`) to run its child engine on a DIFFERENT provider
than the parent, and a provider-selected session propagates its provider to the
sub-agents it spawns (which it now CAN — Half B builds it a per-session Subagent/Team
tool). Precedence: `def.Provider > session-selected provider > build-time default`;
every child routes through `engineDepsForProvider` so it never contaminates the
parent's compactor/counter. Composition-only — the registry never reaches the child
engine (a bare `port.LLMProvider` is handed down).

**Full design: see [`docs/adr/0016-multi-provider.md`](../adr/0016-multi-provider.md)** (registry, catalog-as-data, DTO
neutrality, selection primitive, per-session engine, capability intersection,
disclosure posture + per-client key custody, per-sub-agent provider, and the P0→P3
phasing).

## Related

- [The ports — the LLMProvider seam](ports.md)
- [Context & compaction — per-model context window](context-and-compaction.md)
- [Observability & reliability — provider resilience](observability.md)

---

[← Architecture guide](../architecture.md)
