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
- **P1 (SHIPPED).** A native **Anthropic Messages** adapter (`internal/adapter/anthropic`,
  on the official MIT `anthropic-sdk-go`) — the first provider with a genuinely different
  wire shape, which **VALIDATES the abstraction**. Per-model modality divergence (e.g. some
  Anthropic models take image, some do not) is a pure **data** change: a new registry entry
  whose adapter `Capabilities()` reports its real transmit ability (Image:true, Audio:false,
  EmbeddedContext:true), plus catalog rows whose `inputModalities` differ per model. The
  intersection formula (§7) is unchanged; `engineDepsForProvider`, per-session routing, the
  capability intersection, and per-sub-agent-provider switching ALL work for anthropic with
  **zero new code** (they treat it as data). **No domain/agent/server/acp/proto edit** — the
  only edits are composition (registry + Config + cmd key plumbing). The abstraction held;
  details in §4. Extended thinking is **ON and model-aware**; `max_tokens` is an adapter-
  construction default (catalog-derived per model); `cache_control` is a single ephemeral
  breakpoint at the `StablePrefix` boundary.
- **Live model listing (SHIPPED).** A one-shot **per-provider** live catalog fetch via an
  optional, composition-local `modelLister` capability (§11). OpenRouter is the first
  implementer: its public, **unauthenticated** `/models` endpoint enumerates the real
  catalog (~344 models) and **replaces** the curated embedded subset for that provider on
  success; the embedded catalog is the **fallback floor** on any error/empty/timeout. The
  embedded snapshot is seeded synchronously at `Build` (the `ModelSelection` cap is honest
  from t=0) and the live set is swapped in by a background goroutine — `Build` never
  touches the network.
- **Live metadata → resolvers (SHIPPED).** The live model record now feeds the
  request-path RESOLVERS, not just the picker: a composition-owned `liveMetaStore`
  (seeded from the catalog at t=0, swapped by the SAME one-shot refresh) backs
  live-first-with-catalog-floor lookups for the output ceiling (`max_tokens`), the
  context window, and the Anthropic thinking matrix. The **Anthropic keyed lister**
  (`client.Models.List`) supplies all of these incl. the live thinking descriptor;
  the **OpenRouter** lister now also captures `top_provider.max_completion_tokens`.
  See §12.
- **P2.** Disk cache + **periodic/interval** live refresh; OpenAI lister (sparse —
  catalog-only, see §12); the per-session live-capability closer (so a session bound
  to a *live-only/uncatalogued* model resolves image from live modalities, not the
  adapter-only passthrough fallback). Plus a Chat-Completions adapter for the long
  tail (Gemini-native / Together / …).
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

Because OpenRouter rides the SAME openai adapter, its function tools are sent
**non-strict** (`FunctionToolParam.Strict` left unset). This matters on
strict-enforcing OpenAI-compatible upstreams (e.g. Azure reached via OpenRouter): strict
mode would reject any tool schema whose `required` omits an optional property, and several
built-in tools have optional params. Non-strict avoids the upstream `400`; argument
validation happens at the execution edge (`session.ParseArgs` / `NewToolError`), so strict's
guarantee is not needed. See `IMPLEMENTATION-NOTES.md`.

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
so a re-pin diff shows only real model changes.

The embedded catalog is now the **FALLBACK FLOOR**, not the only source: a provider with
a live `modelLister` (OpenRouter — §11) has its real catalog fetched and **replaces** the
curated subset for that provider; on any fetch error/empty the embedded subset is shown
unchanged. So a keyed OpenRouter picker shows ~344 live models, but offline (or on an
upstream blip) it still shows the curated 19. SSRF hardening for the live fetch shipped
with the lister (fixed-host const URL, keyless, size cap); the SSRF/async-refresh items
formerly parked at P2 are partly delivered (one-shot OpenRouter refresh) — periodic/disk
refresh remains P2.

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

### P1 verdict: the abstraction HELD (`Message.Reasoning` stayed a `string`)

The native Anthropic adapter shipped with **no domain/port/agent change**. The two genuine
wire-divergences P1 surfaced were both absorbed at adapter-construction, not in the DTO:

- **`max_tokens` (required by Anthropic, absent everywhere in the harness)** — resolved
  **per request model**, not baked in: composition injects a `WithMaxTokensResolver(func(model) int)`
  built from the catalog's per-model output limit (`anthropicOutputLimit`), and `buildParams`
  computes `max_tokens` from `req.Model` via it, with a conservative flat fallback
  (`defaultMaxTokens`=4096, the lowest common Claude ceiling) for an uncatalogued model so it
  never 400s. CRITICAL for per-session/sub-agent routing: a route to a smaller-ceiling model
  (e.g. `claude-3-5-haiku`=8192) must not send the default model's larger ceiling. NOT an
  `LLMRequest` field; the adapter stays catalog-free. (The resolver is now LIVE-FIRST
  with the catalog as the floor — §12.)
- **Extended thinking is model-class-dependent — THREE outcomes** (`thinkingConfigFor`):
  `{type:"adaptive"}` for Opus 4.8/4.7/4.6 + Sonnet 4.6 + Mythos (a manual
  `{type:"enabled",budget_tokens}` **400s** on Opus 4.8/4.7); `{type:"enabled",budget_tokens:N}`
  for older thinking-CAPABLE families (Claude 4: Sonnet 4.5/4, Opus 4.5/4.1/4, Haiku 4.5; and
  Claude 3.7 Sonnet); and **NONE — omit `thinking` entirely** for thinking-INCAPABLE models
  (Claude 3.5 and earlier — sending `{type:"enabled"}` there 400s). `display:"summarized"` is
  set explicitly so display deltas stream. The budget is the `WithThinkingBudget` Option
  (clamped ≥1024 & <max_tokens). Thinking is **ON**. The mode is now LIVE-FIRST via
  `WithThinkingResolver` (Anthropic's `Capabilities.Thinking.Types`), with these
  prefix lists kept as the OFFLINE FLOOR — §12.
- **Ambient-env custody** — `New` passes `option.WithoutEnvironmentDefaults()` FIRST, so the
  SDK does NOT autoload `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`/WIF profiles; the adapter
  contributes only the harness-resolved key + optional `--anthropic-base-url`, preserving the
  single-knob credential/base-URL custody and the availability gate.
- **Tool `input_schema` fidelity** — the WHOLE tool schema passes through
  (properties/required on their typed fields, every other top-level key via
  `ToolInputSchemaParam.ExtraFields`), so `$defs`/`additionalProperties`/nested enums/top-level
  constraints survive on Anthropic exactly as on OpenAI.
- **Stream-accumulation cap** — the per-block tool-args / thinking-text / signature buffers
  are bounded (8 MiB) and fail the stream when exceeded (MITM/DoS hardening, matching the
  openrouter lister's caps).

- **Reasoning replay packed into the opaque string.** Anthropic's replay unit is a *list*
  of `thinking` blocks `{thinking,signature}` plus possibly `redacted_thinking` `{data}`
  (interleaved thinking can produce several per turn, and redacted blocks must round-trip
  too). A single bare string cannot hold a list directly, but the `Message.Reasoning`
  contract is explicitly "opaque blob, adapter packs/unpacks its own wire shape", so the
  adapter packs the ordered list **into** the string as a versioned JSON envelope and
  unpacks it to reconstruct the thinking/redacted blocks **before** the `tool_use` blocks
  on the next turn (omitting/misordering them on a tool-bearing assistant turn 400s — the
  load-bearing correctness item). The envelope:

  ```json
  {"v":1,"blocks":[
     {"t":"thinking","x":"<thinking text>","s":"<signature>"},
     {"t":"redacted","d":"<opaque redacted data>"}
  ]}
  ```

  Order in `blocks[]` = the SSE `index` order = the model's original emission order, so the
  thinking-sequence rule is preserved by construction. An empty list packs to `""` (replay
  no-op, exactly like the openai empty-blob case). The adapter emits ONE `ChunkReasoningItem`
  carrying the packed envelope at `message_stop` (honoring "one opaque blob per message"
  even with multiple blocks); the display deltas stream separately as `ChunkReasoning`.
  **`session.Message.Reasoning` stayed a bare `string`** — no domain widening, no leak.

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

**Per-session catalog = core + server-global MCP + client MCP + per-session
Task/Team.** The factory does NOT build a core-only catalog. After `registerCoreTools`
it mounts the **server-global** MCP tools (`cfg.MCPServers` + ToolHive — the same tools
`buildCatalog→registerMCP` mounts on the main engine) by reusing the **shared** manager
Build already connected (`mainMgr.Tools()`), threaded into the factory as `globalMgr`.
This closes the selector-strips-MCP bug: before the fix, selecting any non-default
provider/model (what the mecatui `/models` picker always does) silently dropped every
server-global MCP tool (github/slack/fetch/…). Mount order is **core → global MCP →
client MCP → per-session Task/Team**, which gives **global-wins** collision precedence:
`mcp.Register` is **first-wins + skip-and-continue** — a client tool whose namespaced
name collides with an already-registered global one is SKIPPED (the global tool stays),
and **every other non-colliding client tool is still registered**. This is the
strictly-robust behaviour: a return-on-first-duplicate would have silently dropped every
client tool ordered *after* the collider, and also mis-handled two global servers
clashing. `Register` returns the **skipped tool names** alongside the joined error, so
each mount site emits ONE **provenance-bearing** WARN naming exactly which tools were
dropped and who won — distinct per tier: the per-session client mount says the tool(s)
were *shadowed by an existing server-global tool of the same name (the global tool
wins)* (the line an end-user reads to self-diagnose a vanished tool); the global mounts
(per-session and build-time `registerMCP`) say a *server advertised a name already
registered* (a defective-server / within-global condition). These are composition-layer
logs, so the loop's "exactly two diagnostics lines" invariant does not apply.
**Lifecycle isolation:** `globalMgr` is owned by `Build`; it is reused, never
reconnected, and its `Close` is **NEVER** folded into the per-session
`SessionEngineResult.Close` — a per-session `CloseSession` tearing down the shared
manager would kill MCP for every other live session. `globalMgr` is also the
`reference:`-resolution mainMgr for per-session Task/Team subagent defs (falling back to
the client mgr only when there is no global manager), so a selector session's
`reference: <name>` resolves against the server-global servers — parity with the
build-time path.

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
                       internal/app  (HAS catalog + registry adapters + live meta store)
  reg.meta.modalitiesFor ─► modelCapability(reg, providerID, modelID)
   (LIVE, openrouter)        = ProviderCapabilities{
  catalog.Model ──────►          Image: adapter.Image AND (LIVE-image ELSE catalog-image),
   .InputModalities (floor)      Audio: adapter.Audio AND (LIVE-audio ELSE catalog-audio), (P0 ⇒ false)
  reg.Lookup(pid)                EmbeddedContext: adapter.EmbeddedContext }
   .provider                  (same live InputModalities the picker reads via projectModelEntry)
   .Capabilities() ───►       
                                       │ NEUTRAL values only — no catalog/registry type crosses
                ┌──────────────────────┼───────────────────────────────┐
                ▼                      ▼                               ▼
   (a) ModelInfo.image       (b) CreateSessionResponse           (c) ACP gate
       (ListModels)              .session_capabilities (echo)        Service.ProviderCapabilities()
                                  for the RESOLVED provider+model     = DefaultCapabilities (default)
```

**Modality input is LIVE-FIRST, not catalog-only.** A provider with a live lister
(OpenRouter) stores authoritative per-model `input_modalities` in the `liveMetaStore`
(`modalitiesFor`), and `modelCapability` reads them FIRST: `Image = adapter.Image AND
hasImageModality(live)` (and audio likewise). This is the SAME live
`modelEntry.InputModalities` the picker reads via `projectModelEntry`, so the session
echo / ACP gate and the picker derive image from ONE source and cannot disagree — the
single-source guarantee now spans the live path, not just the embedded catalog. It fixes
the bug where an OpenRouter TEXT-ONLY model (e.g. `openai/gpt-4`) reported `image:true` in
the session echo: OpenRouter shares the openai adapter (`Capabilities()` Image:true) and
nothing read the model's live `["text"]` modalities, so the permissive passthrough default
leaked image. The effect is OpenRouter-scoped: only providers WITH a lister get honest
live gating; openai-direct/anthropic semantics are unchanged.

Precedence (per modality field): (1) LIVE modalities (when the meta store HAS the model —
PRESENCE-keyed: a present entry wins even with an EMPTY modality list, treated text-only
exactly as the picker does, so present-but-empty cannot diverge into echo=true via a
catalogued image row) → (2) embedded CATALOG floor (catalogued model, no live entry) →
(3) ADAPTER-ONLY passthrough (uncatalogued + no live entry). Fail-safe rules (all toward text-only): an
unknown/unavailable provider (or nil registry) ⇒ zero value (a provider we cannot reach
transmits nothing); an uncatalogued or empty `model_id` with NO live entry ⇒ ADAPTER-ONLY
caps (a passthrough model trusts the adapter when both catalog and live are silent —
zeroing would strip image from every passthrough model, and the live-absent permissive
fallback is deliberately preserved).

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
- **Live model listing — shipped:** the optional `modelLister` capability + the OpenRouter
  lister adapter; one-shot background refresh, live-replaces-embedded merge, embedded
  fallback floor, atomic snapshot swap (§11). The SSRF hardening + one-shot refresh moved
  OUT of P2 (they ship here).
- **P2:** disk cache + periodic/interval live refresh; OpenAI/Anthropic listers; the
  per-session live-capability closer; Chat-Completions adapter.
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

## 11. Live model listing (SHIPPED — OpenRouter)

The picker used to show only the curated embedded subset (`providercatalog`, §3) — for
OpenRouter, a hand-pinned 19 of ~344. Live model listing makes a provider whose API can
enumerate its real catalog do so, behind a clean **optional capability** so a future
provider opts in trivially.

**The abstraction.** A composition-local **`modelLister`** interface
(`internal/app/modellister.go`) — `ListModels(ctx) ([]modelEntry, error)` — NOT a `port`,
for the same reason `providerRegistry` is composition-only: a SINGLE consumer
(`liveModelSnapshot`), and the domain/agent never enumerate a catalog (the server still
receives only `[]*mecatlv1.ModelInfo`). `modelEntry` is a NEUTRAL, SOURCE-AGNOSTIC composition-local type
(id, displayName, contextLimit, inputModalities, reasoning, toolCall) — never a `port`
type, never `providercatalog.Model`. Both the live listers AND the embedded floor
(`embeddedModels`) produce `modelEntry`, so a SINGLE projection (`projectModelEntry`) +
sort (`sortModelInfos`) serve BOTH the synchronous seed (`modelSnapshot`) and the live
refresh — the floor and the seed cannot hand-sync-drift. The capability rides on an OPTIONAL
`providerEntry.lister` field (NOT a type-assert on `entry.provider`, because OpenRouter
and OpenAI share the SAME `openai.Provider` adapter and only OpenRouter opts in). **The
whole opt-in for a future provider is: implement `ListModels` + set `entry.lister` at
registry build** — zero merge/snapshot/registry-plumbing change.

**The OpenRouter lister adapter** (`internal/adapter/openrouter`) is a stdlib-only LEAF
(no domain/port/app/other-adapter import; returns its OWN `openrouter.Model`, which
composition maps to `modelEntry` — no import cycle). It GETs the FIXED-host const
`https://openrouter.ai/api/v1/models` over an INJECTED `*http.Client`. Hardening: SSRF —
fixed const URL, no caller-supplied host; CWE-200 — KEYLESS, no `Authorization` header
(the endpoint is unauthenticated; the key never reaches the lister); CWE-770 — an
`io.LimitReader` 4 MiB cap rejects an oversized body; a `ctx` deadline + client timeout
bound a hang. Mapping: `id`→id, `name`→displayName, `context_length`→contextLimit,
`architecture.input_modalities`→modalities, `supported_parameters ∋ {reasoning, tools}`→
reasoning/toolCall.

**Merge + fail-safe.** Per available provider: a SUCCESSFUL, non-empty live result
**REPLACES** the embedded subset for that provider (the point: the real catalog, not the
curated 19 union'd with their live duplicates); ANY error/timeout/empty (or no lister)
falls back to the embedded subset, with one `slog.Warn`. Since every in-scope provider
has an embedded subset, an available provider always contributes ≥ its curated subset —
the picker is **never blanked** by an upstream blip. Availability gating is free:
`liveModelSnapshot` iterates `reg.Available()`, so an unkeyed provider is neither fetched
nor shown (the keyless endpoint does not bypass this — no key ⇒ no entry ⇒ no lister).

**Capability single-source.** The image bit a live model advertises is
`adapterCaps.Image && hasImageModality(model.InputModalities)` — the SAME extracted
`hasImageModality` predicate the embedded path uses (both live and embedded carry a
`modelEntry` with authoritative modalities). So a live text-only model advertises
`image=false` even though the (openai-adapter-backed) OpenRouter provider reports
`Image:true`. **Scope: the PICKER only.** A session bound to a *live-only/uncatalogued*
model still resolves caps via the per-session factory's adapter-only passthrough
(`image=true`) — a documented, bounded picker-vs-session divergence; the closer (a shared
live cache threaded into the per-session factory) is **P2**.

**Async swap.** `Build` seeds `Config.Models` with the EMBEDDED snapshot synchronously
(so `ModelSelection` is honest from t=0 and `Build` NEVER touches the network), then kicks
ONE background goroutine that fetches the live catalog and **atomically swaps** the merged
result via `Service.SetModels` (an `atomic.Pointer[[]*ModelInfo]` inside the Service that
`ListModels`/`ModelSelection` read lock-free). The goroutine is cancelled by `Close`
(no leak; verified under `-race`). A composition-only `liveModelRefreshSync` test seam runs
the refresh inline for deterministic offline e2e (no sleeps). When no available provider
has a lister (mock/openai-only), the refresh is a no-op — no goroutine.

---

## 12. Live metadata → the resolvers (SHIPPED)

§11's live record reached ONLY the picker (`Service.SetModels`). The request-path
metadata that actually drives a turn — the `max_tokens` output ceiling, the
compaction `ContextWindowTokens`, and the Anthropic extended-thinking mode — read
STATIC sources (the catalog, or hardcoded id-prefix lists) on a SEPARATE path the
live data never touched. This slice **broadens the live record so it also feeds those
resolvers**, without leaking the lister/registry/SDK past composition.

**The enriched neutral record.** `modelEntry` (`modellister.go`) gains `OutputLimit`
(the `max_tokens` ceiling) and a `thinkingDescriptor{Known,Adaptive,Enabled}` — the
NEUTRAL, source-agnostic projection of a model's thinking capability. The zero
descriptor (`Known=false`) means "unknown" ⇒ the adapter falls back to its prefix
matrix. Only the live Anthropic source sets `Known=true`; every other source leaves
it zero (a struct of bools costs nothing). The picker proto slice AND the resolver
store both project from the ONE `modelEntry` list per refresh, so they cannot drift.

**The `liveMetaStore`** (`internal/app/livemeta.go`) is a composition-owned
`map[providerID]map[modelID]modelEntry` behind an `atomic.Pointer` (the same lock-free
swap the picker uses). It is **seeded from the embedded catalog at Build BEFORE any
network call** (`seedFromCatalog` over `reg.Available()`), so every resolver has a
correct-enough value at t=0; the SAME background refresh that calls `SetModels` also
calls `store.Swap` with the merged live list. It rides on `providerRegistry.meta`
(every resolver call site already holds `reg`/`provReg`); nil-tolerant reads keep a
hand-built test registry safe.

**Per-field, live-first-then-catalog-floor helpers** replace the bare catalog reads:

| Resolver call site (before)                 | After                                            |
|---------------------------------------------|--------------------------------------------------|
| `WithMaxTokensResolver(anthropicOutputLimit)` | closure over `meta.outputLimitFor(anthropic, …)` |
| `catalogContextWindow(pid, model)` (build/agentdefs) | `reg.meta.contextWindowFor(pid, model)`  |
| `usesAdaptiveThinking`/`thinkingCapable`    | `meta.thinkingFor(…)` via `WithThinkingResolver` |

Precedence is **PER FIELD**: take the live value only when the store has the model
AND the field is present (non-zero / `Known`); else the catalog floor; else the
conservative default the consumer already applies (`engineDepsForProvider`'s 128k,
the adapter's `defaultMaxTokens`). A live MISS for a model the catalog knows falls
back WHOLESALE to the catalog row — **live absence NEVER erases the catalog**. The
per-session child engines (`engineDepsForProvider`/`resolveChildProvider`) pick up
the store FOR FREE through the registry. With NO lister wired the store is
catalog-seeded ⇒ behaviour is **byte-identical** to before (a test asserts it).

**The Anthropic keyed lister** (`internal/adapter/anthropic/lister.go`) is the
reliability headline. It calls `client.Models.ListAutoPaging` and maps the SDK's rich
`ModelInfo` → its OWN neutral `anthropic.Model`: `MaxTokens`→OutputLimit,
`MaxInputTokens`→ContextLimit, `Capabilities.ImageInput`→image, and
`Capabilities.Thinking.Types.{adaptive,enabled}`→`ThinkingDescriptor`. **Auth:** unlike
the keyless OpenRouter lister, this endpoint is AUTHENTICATED — the lister carries the
key for a READ-ONLY metadata GET, used ONLY to read and NEVER logged (CWE-200). It is
availability-gated by construction (it runs only for a keyed = AVAILABLE anthropic
provider). The SDK takes `option.WithHTTPClient` for a mock transport, so the lister
is fully offline-testable (a `testdata/models.json` fixture; no live call ever).

**Thinking-from-live via `WithThinkingResolver`.** A new adapter Option mirrors
`WithMaxTokensResolver`: `thinkingConfigFor` consults the resolver FIRST and, when it
reports `known=true`, TRUSTS the live bits (adaptive ⇒ `{type:"adaptive"}`, else
enabled ⇒ manual `{type:"enabled",budget}`, else NEITHER ⇒ omit thinking = NONE). When
`known=false` (nil resolver, offline, or model absent from the live list) it falls
back to the EXISTING `usesAdaptiveThinking`/`thinkingCapable` **prefix matrix — the
offline floor, which is NOT deleted** (its tests stay green). So a newly-released
Claude model the prefix lists don't know gets its true thinking mode from the API,
while offline/uncatalogued runs keep the deterministic guess.

**OpenRouter live data → resolvers.** The OpenRouter lister now captures the NEW wire
field `top_provider.max_completion_tokens` → `OutputLimit` (a `null` value ⇒ 0 ⇒
catalog floor); its context window + image already arrived but only reached the
picker — they now route through the `liveMetaStore` into the resolvers too. OpenRouter
exposes no thinking-types matrix (only a coarse `reasoning` flag), so a model routed
via OpenRouter leaves the thinking descriptor zero and defers to the adapter floor.

**Per-provider matrix.** Anthropic self-describes EVERY field (output ceiling, context
window, image, thinking) — the authoritative live source. OpenRouter supplies output
ceiling + context + image (no thinking matrix). **OpenAI stays catalog-only**: its
`/v1/models` is sparse (id/created/owned_by only — it cannot self-describe ceilings),
so there is no OpenAI lister and the embedded catalog remains OpenAI's source for
every metadata field.

**Refresh cadence.** ONE-SHOT at Build (reused `startLiveModelRefresh`; no TTL) — a
periodic/disk-cached refresh stays a P2 item. **Not surfaced in the picker:** the
output ceiling / thinking mode remain INTERNAL resolver inputs (no proto/mecatui/caps
change); a TUI badge for them is a deferred follow-up (the picker-proto slice "Slice
D"). The OpenRouter `reasoning_details` replay fix is a SEPARATE request-path bug, not
bundled here.
