# Handover: reasoning-item `id:""` breaks Responses replay against strict gateways

**Status:** IMPLEMENTED + GATED. All 13 steps landed on this branch (uncommitted). Remaining: commit + PR.
**Branch/worktree:** `worktree-fix-reasoning-item-id-replay` (this worktree).
**Done:** fix implemented TDD across `engine/{session,port,agent,adapter/{sessnap,mockllm,eventsource}}`, `provider/openai` (stream capture + request replay), `provider/anthropic` (no-op parity test), `internal/adapter/server` (carryover assertions); invariant pinned in `engine/port/llm_neutral_test.go`; cold reproducer at `provider/openai/reasoning_item_id_fixture_test.go` + `testdata/reasoning_item_id_mixed.sse`; `engine/api/{port,session}.txt` regenerated + `engine/CHANGELOG.md` entry (Added = minor). Gates: `task lint` green; `task test` green except a PRE-EXISTING `TestMatchReadRoot_Lexical` macOS `/var`→`/private/var` flake in `internal/adapter/osfs` (fails on clean HEAD too, unrelated).

---

## TL;DR

mecatl talks to LLM gateways with the **OpenAI Responses API** and is **stateless**
(`store:false`): every turn it resends the whole conversation as `input[]` items.
A reasoning-model assistant turn (gpt-5.x) carries a **reasoning item** forward for
continuity. mecatl preserves the reasoning *blob* (`encrypted_content`) but **throws
away that item's `id`**, and the OpenAI SDK serialises the reasoning item's `id`
field even when empty (`json:"id" api:"required"`, no `omitzero`). So mecatl sends
`"id":""`.

- **Real OpenAI tolerates** the empty required id (uses `encrypted_content`).
- **Strict OpenAI-compatible gateways reject it** → HTTP 400.

Observed symptom (ToolHive LLM gateway = Envoy AI Gateway, model `gpt-5.6-sol`):

```
POST http://127.0.0.1:14000/v1/responses  400 Bad Request
{ "message": "Invalid 'input[6].id': ''. Expected an ID that contains letters,
  numbers, underscores, or dashes, but this value contained additional characters.",
  "type": "invalid_request_error", "param": "input[6].id", "code": "invalid_value" }
```

It only fires **turn 2+** (once a prior reasoning item is in history). Turn 1 works.

---

## Root cause (exact anchors)

The function-call path already preserves the provider item id; the reasoning path
does not. That asymmetry IS the bug.

1. **Capture side — `internal/adapter/openai/stream.go`**
   - `response.output_item.done` → `reasoning`: emits `port.Chunk{Kind: ChunkReasoningItem, Text: item.EncryptedContent}` — **`item.ID` is dropped** (~line 103-112).
   - `response.output_item.done` → `function_call`: emits a `ToolCall` with `ItemID: item.ID` — **id preserved** (~line 128-137). This is the mirror to copy.

2. **Loop aggregation — `engine/agent/loop.go` (~line 1797)**
   - `case port.ChunkReasoningItem:` accumulates `reasoningBlob += chunk.Text`. Nothing captures a reasoning item id (there is nowhere on `port.Chunk` to carry it — see below).

3. **Replay side — `internal/adapter/openai/request.go` `assistantItems` (~line 338-374)**
   - Reasoning item built at ~340-345: `ResponseReasoningItemParam{Summary:{}, EncryptedContent: oai.String(m.Reasoning)}` — **no `ID` set**.
   - Function call at ~360-362: `if call.ItemID != "" { fc.ID = oai.String(call.ItemID) }` — the guard to mirror.

4. **SDK — `github.com/openai/openai-go/v3@v3.37.0/responses/response.go:16811`**
   ```go
   type ResponseReasoningItemParam struct {
       ID string `json:"id" api:"required"`   // <-- NOT omitzero: "" always serialises
       ...
       EncryptedContent param.Opt[string] `json:"encrypted_content,omitzero"`
   }
   ```

5. **`port.Chunk` — `engine/port/llm.go:88`** has NO id field (only `Kind/Text/ToolCall/Usage/Stop`). The function-call id rides `chunk.ToolCall.ItemID`. The reasoning path has no carrier → **a new field is required** (see fix step 1).

6. **`session.Message` — `engine/session/conversation.go:35-55`** has `Reasoning` (blob) and `ProviderPhase` (opaque replay marker). **`ProviderPhase` is the precedent to copy**: an opaque, per-message, wire-omitted-when-empty Responses replay field. Add `ReasoningItemID` the same way.
   - Constructor at `conversation.go:88` (`Reasoning: reasoning`) must also take the new id.
   - `StripReplay` at `conversation.go:~255` clears `Reasoning`/`ProviderPhase`/`ToolCall.ItemID` for provider-neutral history — **must also clear `ReasoningItemID`**.

---

## Proof (both synthetic and real)

Replaying a real reasoning item against the live gateway, varying only the id:

| reasoning item `id` on replay | gateway result |
| --- | --- |
| `""` (what mecatl sends today) | **400** `Invalid 'input[N].id': ''` |
| `"rs_076dbce…"` (the real id) | **200 OK** |
| field omitted entirely | **200 OK** |

Real mecatl traffic captured via a loopback proxy (see "Reproduce" below), turn 2
request (257 KB):
```
input[3] type=reasoning     id=""                         <<< the defect
input[4] type=function_call id="fc_022808fca9714105016a…"  (has its id — the asymmetry)
```
`input[3]` had `encrypted_content` present (1060 chars) and `id:""`.

---

## Proposed fix (mirror the function-call id path)

1. **`engine/port/llm.go`** — add a carrier for the reasoning item id. Simplest:
   add `ItemID string` to `port.Chunk`, documented as "set on ChunkReasoningItem
   (and reserved for other id-bearing items)". (Alternative: a dedicated field.)
2. **`internal/adapter/openai/stream.go`** — on the reasoning `output_item.done`,
   set that id: `port.Chunk{Kind: ChunkReasoningItem, Text: item.EncryptedContent, ItemID: item.ID}`.
3. **`engine/agent/loop.go`** — in `case port.ChunkReasoningItem:` capture
   `reasoningItemID = chunk.ItemID` (last-wins, like the blob) and thread it into
   the assistant `Message` constructor.
4. **`engine/session/conversation.go`** — add `Message.ReasoningItemID` (opaque,
   `json:"reasoning_item_id,omitempty"`, mirror `ProviderPhase` exactly), wire it
   through the constructor, and clear it in `StripReplay`.
5. **`internal/adapter/openai/request.go` `assistantItems`** — stamp it:
   `if m.ReasoningItemID != "" { reasoning.ID = oai.String(m.ReasoningItemID) }`.

### The one real decision: the empty-id edge case
Even after the fix, a reasoning item with **no captured id** (a session persisted
BEFORE this fix, or a provider that sends `encrypted_content` with no id) would
still serialise `"id":""` because the SDK field is `api:"required"`. Going forward
this is moot — every real provider sends an `rs_…` id — so it only affects legacy
sessions. Pick one, document it:
- **(recommended) drop the id-less reasoning item on replay** — lose that turn's
  reasoning continuity but never 400. Cheapest, safe.
- omit the `id` field via a raw/extra-field override (works — proven 200 above —
  but fights the SDK's typed builder; more code).

Do NOT emit a synthetic fake id: unverified whether the backend semantic-checks it,
and it muddies dedup.

---

## Tests to write (invariant-first)

- **Adapter unit (primary), `internal/adapter/openai`:** given a `session.Message`
  with `Reasoning != ""` and `ReasoningItemID == "rs_x"`, `buildInput`/`assistantItems`
  produces a reasoning input item with `id == "rs_x"`. And the regression assertion:
  the built request **never** contains a `reasoning` item with `id:""` (marshal the
  params and grep, or assert the field). Cover the empty-id edge per the decision above.
- **Stream unit:** a `response.output_item.done` reasoning event yields a
  `ChunkReasoningItem` whose new id field == the event item's id.
- **Loop unit (`engine/agent`):** a mock provider streaming a reasoning item with an
  id, followed by a second turn, records `Message.ReasoningItemID` and replays it
  (assert via a capturing mock LLM that inspects the next request's input items).
- Keep it offline: `mockllm` + `memfs`. No live network (repo rule).

A minimal synthetic fixture beats the 257 KB real capture. If you want the real one,
regenerate it (below); it lives outside git (`.scratch/`).

---

## Scope / workflow notes

- **This crosses the engine's public API** (`port.Chunk`, `session.Message`, the
  Message constructor): run `task api:update`, commit the changed `engine/api/*.txt`,
  and add an `engine/CHANGELOG.md` entry classified **Added = minor** per
  `engine/COMPATIBILITY.md` / ADR 0037. The `api-compat` gate fails until you do.
- Gates: `task lint && task test` green; `go run ./cmd/mecademo` still prints a full
  offline session.
- Layering: all changes are within-layer (port stays domain+stdlib; adapter stays in
  `internal/adapter/openai`). No new cross-layer imports.
- Anthropic parity: Anthropic maps its `(thinking,signature)` replay token to
  `ChunkReasoningItem` too (see `port/llm.go:50-54`). Anthropic has no per-item id of
  this kind, so leaving `ReasoningItemID` empty there is correct — just confirm the
  Anthropic adapter doesn't regress (it never set the new field, so it won't).

---

## Reproduce / verify against the live gateway

A throwaway loopback logging proxy already exists in the MAIN checkout at
`/Users/jakub/devel/mecatl/.scratch/thvproxy/` (a separate handover reasons about
promoting it). It captures the exact request bodies. Steps:

1. `thv llm proxy` running (gateway on `127.0.0.1:14000`).
2. Terminal 1: `cd /Users/jakub/devel/mecatl && go run ./.scratch/thvproxy`
   (listens on `127.0.0.1:15000` → forwards to `14000`, saves bodies to
   `.scratch/thvcap/`, flags empty `input[].id`).
3. Terminal 2: `mecatui --toolhive-llm-base-url http://127.0.0.1:15000/v1 --model gpt-5.6-sol`
4. Send a reasoning prompt ("solve the bat-and-ball problem step by step"), let it
   reply, then send a **second** prompt. Watch Terminal 1 flag the empty reasoning id.
5. Inspect: `jq '.input|to_entries[]|{i:.key,type:.value.type,id:.value.id}' .scratch/thvcap/req-*.json`

Pure-curl proof (no mecatl) is in the conversation that produced this doc — POST a
gpt-5.6 request with `include:["reasoning.encrypted_content"]` + `reasoning:{effort:high}`,
grab the returned reasoning item's `id`+`encrypted_content`, then replay it with
`id:""` (400) vs the real id / omitted (200).

---

## Context / related

- **Separate, already-fixed gateway bug:** the gateway used to fail-closed parsing
  the `/v1/responses` `usage` block (503 `cost_enforcement_failure`). That was a
  gateway-side fix (confirmed working now); documented in **merged PR #275**
  (`docs/usage.md` ToolHive troubleshooting). THIS bug is different — a mecatl
  request-shape defect, not a gateway cost issue.
- **Working alias meanwhile:** mecatl's `opencode` provider speaks Chat Completions
  (`internal/adapter/openaichat`, `/v1/chat/completions`), which has no reasoning-item
  replay, so it structurally avoids this. User's workaround:
  `OPENCODE_API_KEY=thv-proxy mecatui --opencode-base-url http://127.0.0.1:14000/v1 --model gpt-5.6-sol`.
- **Do NOT** "fix" this by reclassifying the 400 as retryable, or by adding
  request-body logging to the product (secret/PII leak; use the external proxy).
