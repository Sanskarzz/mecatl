# Multi-provider / multi-model (Phase 0)

mecatl serves more than one LLM provider in a single process and binds a **provider +
model per session**, speaking a provider-agnostic `port.LLMProvider` behind a
server-side registry. All of the multi-provider wiring lives in the composition layer
(`internal/app`); the domain, the agent loop, and the server/acp adapters never see a
registry or a catalog — they receive a bare `port.LLMProvider` or a neutral value.

This document is the design rationale. The runtime overview is `docs/architecture.md`
§18 (which cross-links here).

---

## 1. Purpose & phasing

- **P0 (shipped).** Registry + env-credential detection + embedded model catalog +
  availability-gated picker + a two-field provider/model selector + a per-session
  engine + a DTO-neutrality audit + capability single-source + disclosure hardening.
  Providers: **OpenAI** and **OpenRouter**, both on the SAME stateless Responses
  adapter (OpenRouter = the openai adapter with the OpenRouter base URL substituted).
- **P1 (next).** A native **Anthropic Messages** adapter — the first provider with a
  genuinely different wire shape, which VALIDATES the abstraction. Per-model modality
  divergence (e.g. some Anthropic models take image, some do not) is a pure **data**
  change: a new registry entry whose adapter `Capabilities()` reports its real transmit
  ability, plus catalog rows whose `inputModalities` differ per model. The intersection
  formula (§7) is unchanged; no server/acp/proto edit.
- **P2.** Async catalog refresh (with SSRF hardening) + a Chat-Completions adapter for
  the long tail (Gemini-native / Together / …).
- **P3.** Secrets store + OAuth + per-client/profile key custody (see §8).

---

## 2. The provider registry (`internal/app/registry.go`)

`buildProviderRegistry` constructs, ONCE at `Build`, the set of **available** providers.
A provider is available iff one of its credential env vars resolves; the var NAMES come
from the embedded models.dev catalog (`providercatalog`), with one composition-layer
augmentation: OpenRouter also accepts `OPENAI_API_KEY` by mecatl convention (it rides
the same Responses adapter). Only available providers are constructed and held — there
is no point holding an unkeyed provider, and its very availability is sensitive (§8,
CWE-200). The registry is composition-only; it is **not** a `port` (a single consumer),
mirroring the `envDetector` seam. `UseMock` short-circuits to a single synthetic `mock`
entry (offline); the zero-keys case is the named, actionable `errNoProvider`.

`buildProvider` returns the registry **and** its default provider, so the shared engine
and every child/fork/team/dream/reviewer engine keep receiving the single default
provider exactly as before — the default path is byte-identical to pre-multi-provider.

A composition-only `providerConstructor` seam (mirroring `envDetector`) lets the offline
e2e back two real provider ids with distinct mocks; production leaves it nil and uses
the resilience-wrapped openai adapter.

---

## 3. Catalog-as-data (`internal/adapter/providercatalog`)

The model catalog is an embedded, pinned, hand-curated SUBSET of the community
[models.dev](https://models.dev/api.json) snapshot (MIT-licensed; the full license +
attribution is vendored in `MODELS_DEV_LICENSE`). It is a stdlib-only LEAF data adapter:
`embed` + `encoding/json` only, no domain/port/app/other-adapter imports. Its
`Catalog`/`Provider`/`Model` are package-own value types and must never leak into the
domain or port — only the composition layer reads them.

Curation is explicit and reviewable (no silent caps, no regex sweep): openai (all 52),
anthropic (all 24, made ready for P1), openrouter (a 19-route flagship allowlist). An
unknown provider/model id is an honest `(_, false)` lookup miss, never a substitution.
Regeneration is one deterministic `jq -S` filter (recorded in the package doc-comment)
so a re-pin diff shows only real model changes. Async refresh + SSRF hardening are P2.

---

## 4. DTO-neutrality principle

The uniform internal format is `port.LLMRequest` at the `port.LLMProvider` seam. It is
deliberately provider-NEUTRAL and guarded:

- `Model` is a **bare opaque string** — no provider/endpoint/key rides the request;
  those are server-side registry concerns.
- Provider-PRIVATE knobs (OpenAI store/include flags, an Anthropic thinking budget, a
  reasoning effort) are an **adapter-construction** concern — a `WithThinkingBudget`-style
  Option like the existing `openai.WithBaseURL`, NOT a new `LLMRequest` field. The
  domain/agent loop never branches on provider.
- Reasoning replay is uniform in STRUCTURE (one opaque blob per message via
  `ChunkReasoningItem`), provider-private in CONTENTS (OpenAI `encrypted_content`,
  Anthropic `(thinking,signature)`). The display summary (`ChunkReasoning`) vs replay
  blob split is the neutral seam P1 validates — do not collapse it.
- A reflection guard (`internal/port/llm_neutral_test.go`) tripwires any silent
  `LLMRequest` field addition.

---

## 5. Provider/model selection primitive

The wire surface is **two proto fields** — `provider_id` + `model_id` on
`CreateSessionRequest` — NOT a slash-joined string (an opaque pair avoids a parsing/
escaping seam). At the server boundary they map to the neutral
`server.ProviderSelector` value object; the server adapter imports neither the registry
nor the catalog. The composition root resolves the selector. Resolution table:

| `provider_id` | `model_id` | Outcome |
|---|---|---|
| `""` | `""` | **Shared engine** (default provider, no per-session build) — byte-identical to today |
| `""` | set | **InvalidArgument** — a bare model on the env-derived default provider is ambiguous |
| known+available | `""` | per-session engine on that provider's default model |
| known+available | catalogued | per-session engine bound to (provider, model) |
| known+available | NOT catalogued | **passthrough** — the model string reaches the provider verbatim (catalog gates nothing) |
| unknown/unavailable | any | **InvalidArgument** — `"unknown or unavailable provider"`, never a silent fallback |

The provider is **fixed for the session lifetime** (reasoning-replay + the byte-stable
cache prefix are provider-private; "switch provider" = a new session). Deferred:
settings `default_model`, client last-used persistence (these are P-later / client-side).

---

## 6. Per-session engine + `engineDepsForProvider`

The widened `SessionEngineFactory func(ctx, sel, specs) (SessionEngineResult, error)`
is the ONE seam for a per-session engine — it serves BOTH a non-default provider/model
AND client-provided streaming-HTTP MCP servers (orthogonal inputs → ONE engine over ONE
catalog). The composition factory resolves the selector against the registry and builds
Deps via **`engineDepsForProvider`**, which re-derives EVERY provider/model-closing
field — LLM, Compactor, Model, model-keyed TokenCounter, `PromptConfig.Env.Model`, and
the **`ContextWindowTokens`** (looked up from the catalog's `ContextLimit()` for the
selected model so the compaction trigger AGREES with the `ListModels`-advertised
`context_limit`; an uncatalogued passthrough model or the default provider falls back to
the 128k default). This is the cross-provider contamination guard: a shallow clone
swapping only the LLM would compact and count through the wrong model.

`session.Session` is NOT widened — the selector resolves to an ENGINE at create time,
registered in the same `sessionEngines` map (and selected the same way by
`StartRunContent`) the client-MCP path uses. The map is **capped** at
`Config.MaxSessionEngines` (default 1024): the gRPC/HTTP surfaces have no
connection-teardown drain, so without a cap a client creating selector sessions and
never calling `CloseSession`/`EndSession` could grow it unbounded (CWE-770). Past the
cap, `createSession` returns `ErrTooManySessionEngines` (gRPC `ResourceExhausted` /
HTTP 429); `CloseSession`/`EndSession` frees a slot.

`SessionEngineResult` (a struct, not a tuple) carries the built engine, the per-session
resolved **capabilities** (§7), and the MCP close func. The struct keeps the two
interface-typed members readable and leaves room for future per-session metadata
without another signature churn.

---

## 7. Capability single-source / intersection (`internal/app/capability.go`)

**The defect this fixes.** Capability truth previously came from two disconnected reads:
`ModelInfo.image` was the catalog ALONE (the wired adapter's transmit ability was never
consulted), and `Service.capabilities()` / `Service.ProviderCapabilities()` both read
the SHARED engine's provider — so a session bound to a DIFFERENT provider/model via the
per-session factory got capability bits describing the wrong engine, and the ACP gate
had the same blind spot.

**The fix.** `modelCapability(reg, providerID, modelID) port.ProviderCapabilities` is
the SINGLE SOURCE: the INTERSECTION of the catalog's per-model modalities and the wired
adapter's `Capabilities()`. The adapter is the AUTHORITY on what it can actually
TRANSMIT; the catalog is the authority on what the model ACCEPTS; the AND of the two is
the honest truth. It lives in composition (`internal/app`) — the only layer holding both
inputs. The result is a NEUTRAL `port.ProviderCapabilities`; neither the catalog nor the
registry type crosses into the server/acp adapters.

```
                       internal/app  (HAS catalog + registry adapters)
  catalog.Model ──────► modelCapability(reg, providerID, modelID)
   .InputModalities      = ProviderCapabilities{
  reg.Lookup(pid)            Image: adapter.Image AND catalog-image,
   .provider                 Audio: adapter.Audio AND catalog-audio,  (P0 catalog has no audio ⇒ false)
   .Capabilities() ───►      EmbeddedContext: adapter.EmbeddedContext }
                                       │ NEUTRAL values only — no catalog/registry type crosses
                ┌──────────────────────┼───────────────────────────────┐
                ▼                      ▼                               ▼
   (a) ModelInfo.image       (b) CreateSessionResponse           (c) ACP gate
       (ListModels)              .session_capabilities (echo)        Service.ProviderCapabilities()
                                  for the RESOLVED provider+model     = DefaultCapabilities (default)
```

Fail-safe rules (all toward text-only): an unknown/unavailable provider (or nil
registry) ⇒ zero value (a provider we cannot reach transmits nothing); an uncatalogued
or empty `model_id` ⇒ ADAPTER-ONLY caps (a passthrough model trusts the adapter when the
catalog is silent — zeroing would strip image from every passthrough model).

**The three sinks share one value:**
- (a) `modelSnapshot` sets `ModelInfo.Image = modelCapability(...).Image`.
- (b) the per-session factory computes `modelCapability` for the resolved
  (provider, model) and returns it on `SessionEngineResult.Capabilities`; the Service
  stores it on `sessionEngine.caps` and echoes it verbatim on the new proto
  `SessionCapabilities { bool image = 1; bool audio = 2; }`
  (`CreateSessionResponse.session_capabilities`). For the zero-selector shared-engine
  path (which never calls the factory) the echo reads `Config.DefaultCapabilities` —
  the composition-computed intersection for the default provider + `cfg.Model`.
- (c) `Service.ProviderCapabilities()` returns the SAME `Config.DefaultCapabilities`,
  so the ACP gate and the wire echo for a default-engine session cannot disagree.

**A dedicated `SessionCapabilities` message** (not a reuse of `ServerCapabilities`, not
two bare bools) keeps the per-session surface MINIMAL: only the model-varying input axis
(image/audio) belongs per session; the other ~10 `ServerCapabilities` bits
(mcp/skills/teams/…) are server-wide and would be misleading or duplicated per session.
The field is additive — an older server leaves it absent and the client falls back to
`capabilities`.

**Reasoning is intentionally EXCLUDED from the echo and the intersection.** It is a
`ModelInfo` field (catalog-sourced), not a `port.ProviderCapabilities` bit, and there is
no adapter "can replay reasoning" authority bit in P0. If P1 wants reasoning intersected,
add the adapter bit then — do not invent it speculatively.

### Right-sized OUT of P0 (deferred to P1)

- **Per-session ACP capability gate.** ACP carries NO per-session provider/model
  selector in P0 (`session/new` passes only `mcpServers`, never a selector), so every
  ACP session rides the DEFAULT engine and the Agent's capture-once
  `a.caps = svc.ProviderCapabilities()` is correct for every ACP session — provided (as
  now) `ProviderCapabilities()` returns the intersected default caps. The gate moves from
  capture-once to per-session lookup only when an ACP selector lands (P1+). Plumbing a
  per-session ACP gate now would be speculative work with no P0 caller.
- **Reasoning-capability intersection** — see above.
- **Audio catalog modality** — the catalog has no explicit audio field today; `Audio`
  derives from the raw input-modality list (false in P0 data) AND-ed with the adapter
  (P0 adapter `Audio:false`), so the result is false regardless. No catalog schema
  change in P0.

---

## 8. Disclosure posture & per-client key custody (CWE-200)

API keys are **operator-supplied, server-side, env-only** for Phase 0. A key is read
once at startup by the composition root (`internal/app`), held only inside the
resilience-wrapped `port.LLMProvider` in the server-side provider registry, and is NEVER
placed on any wire, in any proto message, log line, or client-visible field. A remote
client selects a provider by an **opaque `provider_id`** and a model by an **opaque
`model_id`**; it receives back only public catalog metadata + boolean capabilities. A
provider with no resolved credential is omitted from `ListModels` entirely — its very
availability is concealed (CWE-200). There is no per-client key: all authenticated
clients share the operator's server-side keys; tenant isolation and per-client /
secret-store / OAuth key custody are **P3**.

### Auth-gating guarantee

`ListModels` is behind auth on BOTH surfaces: the gRPC `UnaryInterceptor` wraps ALL
handlers method-agnostically, and the HTTP `auth.Middleware` wraps the whole mux. An
explicit test (`TestListModelsRequiresAuth`, gRPC + HTTP) pins this against a future
handler that might bypass the interceptor — an unauthenticated `ListModels` would
otherwise leak the available-provider set.

### Log-audit checklist (every new/changed slog line on the multi-provider path)

| Line | Logs | Verdict |
|---|---|---|
| `registry.go` mock warn | a fixed string | clean (no key/URL) |
| `registry.go` "LLM provider available" | `provider` (id), `model`, `base_url` | clean — base URL is a plain host (OpenRouter `https://openrouter.ai/api/v1`; OpenAI operator-set), never a userinfo/`?key=` credentialed form |
| `registry.go` "LLM resilience enabled" | knobs (attempts/timeouts) only | clean |
| `build.go` client-MCP warns/info | `server`/`url`/counts/`err` | clean — the URL is the client-supplied MCP server URL, not an LLM-provider credentialed URL |
| `grpc.go` / `http.go` CreateSession echo | nothing logged | clean — `session_capabilities` is bools-only and unlogged |

`TestNoKeyInStartupLogs` captures the slog output of a real `buildProviderRegistry` with
a sentinel key and asserts the key appears in no line. `TestModelSnapshotNoSecrets`,
`TestSessionCapabilitiesNoSecrets`, and the multi-provider e2e tripwire the sentinel
across `ListModels` and the `CreateSessionResponse` (incl. the capability echo). Verdict:
**clean** — no key or credentialed URL on any wire, proto, or log surface.

---

## 9. Phasing recap + open/deferred

- **P0 — shipped:** registry, env-detect, embedded catalog, availability-gated picker,
  two-field selector, per-session engine, DTO audit, capability single-source/
  intersection, disclosure hardening. OpenAI + OpenRouter.
- **P1:** native Anthropic Messages adapter (validates the abstraction); per-model
  modality divergence is a data change; per-session ACP gate + (if wanted) reasoning
  intersection land here.
- **P2:** async catalog refresh + SSRF hardening; Chat-Completions adapter.
- **P3:** secrets store + OAuth + per-client/profile key custody + tenant isolation.

Model-selection persistence is **per-workspace only** (realpath-keyed); a pick never
writes the global `default`, so an unseen repo falls back to the server default rather
than inheriting the last pick made elsewhere. An explicit "set as default" affordance
(the only writer of the global `default`) is a deferred follow-up.

Open / deferred items: an explicit "set as default" gesture, a `small_model` tier
(sub-agent cheap model), mid-session same-family model switch, and a zero-keys
first-run UX.

## 10. Per-sub-agent provider selection (SHIPPED — both halves)

A Task agent definition and a team member may resolve to a DIFFERENT provider (+
model) than the parent, routed through the same `providerRegistry` — without leaking
the registry past the composition layer (`internal/app`). Both halves are SHIPPED:

- **Half A — def-pinned provider.** A new `provider:` frontmatter field on an agent
  def (pure data on `agents.AgentDef.Provider`; the adapter never imports the
  registry) routes that def's child engine to the named provider. It is orthogonal
  to `model:` (two fields, mirroring the wire's `provider_id`/`model_id` — never a
  slash-joined string).
- **Half B — session-provider propagation.** A session that SELECTED provider P over
  the Phase-0 wire now gets a per-session Task tool (and, under `--enable-teams`, an
  in-catalog Team tool) wired to P as the inherited parent — so its sub-agents that
  pin NO provider inherit P, not the build-time default. The build-time per-session
  catalog was core-tools-only, so a selected session previously could not spawn
  sub-agents at all; Half B closes that gap by reusing the SAME `buildTaskTool`/
  `buildTeamWiring` builders (no per-session-catalog drift), folding the Task tool's
  inline-MCP close into `SessionEngineResult.Close`, bounded by `MaxSessionEngines`.

**Three-level provider precedence:** `def.Provider > session-selected provider >
build-time default provider`, realised by a `parentProviderID` the call site threads
into the one shared resolver `resolveProviderModel(cfg, reg, def, parentProviderID,
parentModel)` (the build-time path passes `reg.Default()`/`cfg.Model`; a selected
session passes its resolved provider/model). When the provider SWITCHES, the model is
rebased off `def.Model` (or the new provider's `builtinDefaultModel`), NEVER the
inherited parent model (a bare `gpt-5` is invalid on openrouter). Same-provider keeps
the existing `resolveModel` chain (full back-compat).

**Contamination fix:** every child engine — def-pinned OR session-inheriting — is
built through `newChildEngineForProvider` → `engineDepsForProvider`, so a child on
provider X compacts/counts/prompts through X with X+model's catalogued context
window. A non-switching child keeps window=0 (128k, byte-identical).

**Fail-safe:** a def naming an unknown/unavailable provider is a LOUD fallback to the
parent provider + `slog.Warn` (mirroring every other forgiving def-error handler) —
one bad shared-repo def never wedges startup.

**Deferred (NOT built):** (1) the standalone gRPC `CreateTeam` RPC's per-session
provider — `server.Config.MemberEngine` is wired ONCE at build with the default
provider and CreateTeam carries no selector today; the in-catalog Team tool IS
covered. (2) Surfacing the resolved provider in `ListAgents`/`AgentInfo` — that is a
proto change with no consumer yet.
