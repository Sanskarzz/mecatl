# mecatui — the terminal UI for mecatl

`mecatui` is a flashy, themeable terminal UI for the mecatl harness. It is a
**gRPC client**: it creates a session, opens the bidi `Converse` stream, renders
the streamed events (glamour markdown for assistant text, themed lipgloss cards
for user prompts and tool I/O), shows a thinking spinner and a status/usage
footer, and resolves permission prompts inline by sending `ResumeApproval` back
on the same stream.

The server it talks to is either an **external** `mecated` (pass `--server`) or,
by default, one `mecatui` **hosts itself in-process** over a private UNIX socket
— so a single binary "just works" with no daemon to start and no TCP port. See
[Run](#run).

It is built on the Charm v2 stack (Bubble Tea / Lip Gloss / Bubbles / Glamour).
The render packages (`ui`, `theme`) and the `client` package stay a pure client —
they never import any `internal/...` package and render solely from the proto
`Event` envelope. Hosting the embedded server is confined to the `cmd/mecatui`
main and its `embed` subpackage (which build the same server `mecated` does, via
`internal/app`).

## Build

```sh
task build          # → bin/mecated, bin/mecademo, bin/mecatui
```

## Run

The simplest path needs no separate server — just launch the TUI with an LLM key:

```sh
# Embedded server (default): mecatui hosts mecated in-process over a UNIX socket.
# The provider is AUTO-DETECTED from whichever key is set:
OPENAI_API_KEY=sk-...      bin/mecatui --workspace "$PWD"   # OpenAI
OPENROUTER_API_KEY=sk-or-... bin/mecatui --workspace "$PWD" # OpenRouter

# Offline, no network — uses the canned mock provider:
bin/mecatui --mock --workspace "$PWD"
```

With no `--server`, mecatui runs in **auto** mode: it first probes the loopback
default `127.0.0.1:8080` and **reuses a `mecated` already running there**; only if
none answers does it host an **embedded** server itself (a UNIX socket in
`$XDG_RUNTIME_DIR`, torn down on exit). The embedded provider is **auto-detected**
from the environment — `OPENAI_API_KEY` enables the `openai` provider,
`OPENROUTER_API_KEY` the `openrouter` provider (set both, and you pick between their
models in the **`/models`** picker — see the Overlays section); with neither, the
offline mock (`--mock`). When more than one is keyed, run `/models` to choose; the
choice is persisted per workspace.

To use a specific **external** server instead, pass `--server`:

```sh
bin/mecated &                                          # listens on 127.0.0.1:8080
bin/mecatui --server 127.0.0.1:8080 --workspace "$PWD"
```

`--workspace` defaults to the current directory and is always resolved to an
absolute path (the server requires absolute).

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--server` | – (auto) | external mecated `host:port`; empty = reuse `127.0.0.1:8080` if running, else embed |
| `--workspace` | cwd | absolute session workspace root |
| `--mode` | `default` | permission posture: `default` \| `plan` \| `accept-edits` |
| `--theme` | `aztec` | theme name (also `MECATUI_THEME`) |
| `--theme-dir` | – | extra directory of `*.json` themes to load |
| `--auth-token` | – | bearer token for an **external** server (or `MECATL_AUTH_TOKEN`) |
| `--tls` | off | use TLS transport for an **external** server |
| `--tls-ca` | – | PEM CA bundle for external-server verification |
| `--insecure` | off | skip TLS verification (testing only) |
| `--list-themes` | – | print available themes and exit |
| `--model` | – (provider default) | model id for the **embedded** server; empty = the provider-appropriate default (openai → `gpt-5`, openrouter → `openai/gpt-5`). Overridden per session by the `/models` picker |
| `--openai-base-url` | – | OpenAI base URL override for the **embedded** server |
| `--openrouter-base-url` | – | OpenRouter base URL override for the **embedded** server (default `https://openrouter.ai/api/v1`) |
| `--mock` | off | **embedded** server: use the offline mock provider (no network) |
| `--no-bash` | off | **embedded** server: disable the Bash tool (shell-less) |
| `--memory-dir` | – (auto) | **embedded** server: per-project memory store dir; empty = a default under `$XDG_DATA_HOME/mecatui/memory` |
| `--no-memory` | off | **embedded** server: disable cross-session memory (Remember/Recall) |
| `--commands-dir` | – (auto) | **embedded** server: slash-command template dir; empty = the conventional `.mecatl/commands`, `.claude/commands` |
| `--no-commands` | off | **embedded** server: disable slash-command expansion |
| `--skills-dir` | – (auto) | **embedded** server: skill-unit dir (`<name>/SKILL.md`); empty = the conventional dirs (e.g. `.claude/skills`) |
| `--no-skills` | off | **embedded** server: disable skill discovery (the Skill tool) |
| `--trust-project` | off | **embedded** server: honour a discovered project's permission **ALLOW** rules **and** its project soul (`.mecatl/soul.md`). Default OFF, unified with `mecated` — deny/ask are always honoured regardless. Only pass it for a repo you trust |
| `--perf` | off | **embedded** server: expose the loopback perf admin surface (`/metrics`, `/debug/pprof`, `/debug/vars`, `/debug/flightrecorder`) and wire domain metrics. Loopback, UNAUTHENTICATED |
| `--perf-addr` | – (`127.0.0.1:9099`) | **embedded** server: admin listen address for `--perf`. Empty = the **fixed** `127.0.0.1:9099` (predictable, so an MCP-client config can hardcode the `/mcp` URL; distinct from `mecated`'s `:9090`). Pass another `host:port`, or `127.0.0.1:0` for an ephemeral port. On a clash, startup **fails with guidance** |
| `--perf-mcp` | off | **embedded** server: mount the read-only perf MCP server at `/mcp` on the `--perf` surface (introspect this process over MCP). Refuses a non-loopback `--perf-addr` |

The embedded server has no auth/TLS — it is a private, user-owned UNIX socket
(the same single-user loopback trust model `mecated` uses for `127.0.0.1`, with a
tighter blast radius). The `--auth-token` / `--tls*` flags apply only when dialling
an external `--server`; loopback is unauthenticated plaintext by default, matching
mecated's trust model. The embedded server keeps the heavier opt-ins (MCP,
ToolHive, the writable SkillDraft quarantine) **off** — for those, run a full
`mecated` and point `--server` at it.

**Skill discovery is ON by default**, via conventional discovery (the read-only
`Skill` tool activates progressive-disclosure `<name>/SKILL.md` units from the
conventional dirs, e.g. `.claude/skills`, when present) — consistent with
agent-definition discovery. Skills register only when at least one `SKILL.md` is
found (opt-in by presence), so with none, `caps.Skills` is false and the `?`
overlay reflects that. When skills ARE discovered, `/skills` opens a read-only
inventory panel listing each skill's name + one-line description (a startup
snapshot via the `ListSkills` RPC — skills are immutable for the process
lifetime). Activation stays the model's call (the `Skill` tool reads the body on
demand); the panel is discovery only. Pass `--no-skills` to disable discovery entirely, or
`--skills-dir` to scope it to a single vetted directory. Note the trust boundary:
a skill auto-activates from its always-in-context metadata, so a `SKILL.md` in a
workspace you didn't author (e.g. a cloned repo's `.claude/skills`) can steer the
model — use `--no-skills` for untrusted workspaces. Only read-only discovery is
wired; the writable SkillDraft self-improvement loop stays off.

**Project trust posture (incl. the project soul).** The embedded server does **not**
blanket-trust the workspace you launch it in. `--trust-project` is **default OFF**,
unified with `mecated` (WORKSPACE-TRUST Phase 0). So, by default, a repo's
`.mecatl/settings.yaml` permission **ALLOW** rules are ignored and a project
persona/soul at `<workspace>/.mecatl/soul.md` (issue #14, Phase 3) is **not** loaded;
the repo's deny/ask permission rules are always honoured regardless, and your
user-scoped soul (`~/.config/mecatl/soul.md`) always loads and takes precedence.
Pass `--trust-project` for a repo you trust to honour its ALLOW rules and project
soul — exactly the gesture `mecated` requires. (Earlier builds hardcoded trust ON
for the TUI; that blanket-trust regression is gone.)

**First-encounter trust prompt (WORKSPACE-TRUST Phase 2c).** Rather than silently
ignoring an untrusted repo, the embedded server asks you once. Before the TUI takes
over the screen (a **pre-alt-screen** stderr prompt, alongside the other startup
notices), if the workspace is **not already trusted** and carries a **project
authority set** worth gating — a project soul, project-tier agents/commands/skills,
or a `settings.yaml` with **ALLOW** rules — `mecatui` prompts:

```
mecatui: do you trust the project files in this workspace?
  /home/me/src/some-cloned-repo
[t]rust (persist) / [o]nce (this run only) / [n]o (default):
```

- **`t`** trusts this run and **remembers** it (writes the workspace + its current
  identity-anchor hash to `~/.config/mecatl/trust.yaml`, so it stays trusted until
  the project's identity surface drifts).
- **`o`** trusts this run only (nothing persisted).
- **`n` / Enter / anything else** declines — the safe default (project authority
  withheld; the agent still runs with your user-tier config + built-in tools).

If you had trusted the repo before and its **soul / agents / commands / skills
changed since**, the prompt **re-fires** as a drift re-prompt ("this workspace
**CHANGED** since you trusted it"). A repo with nothing to gate (no project
authority, or only deny/ask rules) is **never** prompted, and an already-trusted
repo (`--trust-project`, `trustedWorkspaces:`, or a remembered + undrifted entry) is
**not** re-asked. If stdin is **not a terminal** (piped/headless), `mecatui` cannot
prompt — it proceeds **untrusted** for that run and never blocks. The echoed path is
terminal-escape-sanitized (CWE-150). The prompt lives in the `mecatui` composition
root, not the render layer — no proto event, no change to `ui`/`theme`/`client`. See
`docs/usage.md` for the full semantics; `mecated` itself never prompts (declarative).

**Built-in client-side slash commands always appear.** Typing `/` opens the
palette with a set of commands the TUI itself ships — independent of workspace
dirs and even when server slash-command expansion is off. `/clear` (reset the
conversation and scrollback) and `/help` (open the keys-&-features overlay) are
*always* available because they act purely on the TUI's own state; `/mcp` (browse
the MCP inventory), `/agents` (browse the agent-definition inventory — the
resolved registry the `Task` tool routes delegations to), `/team` (the live
agent-team overlay, also on `ctrl+a`), `/skills` (browse the skills inventory),
`/soul` (inspect the persona — read-only), `/usermodel` (inspect the user
model — read-only), and `/models` (pick the model for the next session) appear
only when the connected server advertises those capabilities (and, for
`/mcp`/`/agents`/`/skills`/`/soul`/`/usermodel`/`/models`, the matching client
collaborator is wired). The fixed palette order is
`clear, help, mcp, agents, team, skills, soul, usermodel, models` (locked by a test).
`/agents` and `/team` are distinct: `/agents` is the **definition inventory** (a
palette-only `ListAgents` snapshot, gated on `caps.agents`), while `/team` opens
the **live overlay** of a team that has actually run (gated on `caps.teams`).
These never reach the model — a bare built-in line is intercepted and run
locally. (`/compact` is a planned follow-up: it needs a server RPC that does not
exist yet.)

**`/soul` (read-only persona inspection).** Gated on `caps.soul` AND a wired soul
fetcher. It fires `GetSoul` (a build-time snapshot the server takes once at
startup) and shows a metadata line — provenance + trust state
(`user` | `project (trusted)` | `project (UNTRUSTED → not loaded)` |
`· DRIFTED`), byte size, and a short content hash — over the persona body. Because
the body can be up to 20 KiB, the panel is **scrollable**: `pgup`/`pgdn` (and
`up`/`down`, `home`/`end`) move a line-window, with a "lines X–Y of N" indicator
when the content overflows the window; `esc` closes. It NEVER edits the soul (the
soul is agent-read-only); trust/drift are computed server-side in composition and
only displayed here.

**`/usermodel` (read-only user-model inspection).** Gated on `caps.user_model` AND
a wired user-model lister. It fires `GetUserModel` (a **live** read of the
user-model store's index, so it reflects facts saved since startup) and shows an
aggregate line (count · size · hash) over a `key — description` list, name-sorted.
The per-entry value is omitted — `RecallUser` loads it. `esc` closes; the panel
never edits the user model (the agent curates it).

**`/models` (model picker — the only *selecting* overlay).** Gated on
`caps.model_selection` (the server advertises ≥1 available provider) AND a wired
model lister. It fires `ListModels` and renders the selectable models **grouped by
provider**, each row showing the display name plus capability glyphs (`img` when
the model takes image input, `reason` when it emits reasoning) and a compact
context window (e.g. `200K`, `1M`; omitted when unknown). `↑`/`↓` move a cursor
across the flattened list, `enter` **selects** the cursor model (a `●` marks the
currently-active one), and `esc` closes. UNLIKE the read-only overlays it changes
state: selecting **persists** the choice (last-used) and **applies to the NEXT
session** (`provider_id`/`model_id` on the next `CreateSession`) — it does NOT
re-route the live session (provider is fixed per session; a live switch is a
deferred follow-up). The pick is persisted **client-side** to a state file:
`$XDG_STATE_HOME/mecatui/models.yaml` (fallback `~/.local/state/mecatui/models.yaml`)
— a per-workspace map (realpath-keyed). A pick is scoped to its workspace only; an
unseen/new repo falls back to the server default rather than inheriting another
repo's pick. On launch the selection is **reconciled** against
`ListModels` BEFORE the first `CreateSession`: if the persisted model's provider is
no longer available (its key was removed), the selection falls back to the server
default for that run with a loud notice and the state file is left intact (the
preference returns next launch). The state file is machine-written **state** under
`XDG_STATE_HOME`, a sibling of the human config — the same settings-vs-state split
as `trust.yaml`.

**Workspace slash commands are ON by default** on top of the built-ins, expanding
`/<name>` inputs from the conventional workspace dirs `.mecatl/commands` and
`.claude/commands` (`<name>.md` templates — the Claude Code convention). They're
local, user-authored prompt templates, so there's no network or trust cost (unlike
MCP prompts, which stay off with MCP). Pass `--no-commands` to disable expansion,
or `--commands-dir` to point at a different directory. Built-ins and workspace
commands merge in the palette (a built-in wins a name collision). When no
workspace command dir exists the palette is **not** empty — the built-ins are
still there; a typed prefix that matches nothing shows a muted "no matching
command" note.

**Cross-session memory (Remember/Recall) is ON by default**, scoped per-project
under `~/.local/share/mecatui/memory/<path-slug>/` (or `$XDG_DATA_HOME/...` when
set), where `<path-slug>` is the absolute workspace path with `/` replaced by `-`
(e.g. `-var-home-ozz-dev-mecatl`) — deterministic, human-legible, and collision-free
across same-named checkouts. Pass `--no-memory` to disable it or `--memory-dir` to
relocate the store. Background memory consolidation (the "dream" distiller, which
spends tokens) stays **off** on the embedded server.

### `@`-file mentions and media attachments

Typing `@` opens an inline **file-completion menu** — the same kind of dropdown as
the `/` palette, mutually exclusive with it (a line is either a `/command` or has an
`@token` word, never both). It lists workspace files matching the typed token
(case-insensitive substring on the path or base name; dotfiles/`.git` pruned; capped
to 8 rows and a bounded directory walk so a huge tree never blocks). `↑`/`↓` select,
`tab`/`enter` complete the highlighted path into the input (`@<path> `), `esc`
dismisses.

On submit, every `@path` in the prompt is read and routed by sniffed content type:

- an **image** or **audio** file becomes an inline media part sent over gRPC
  (`Prompt.parts`), rendered in the transcript as a `📎 image/png (inline)` line;
- any **other** file is treated as text and **inlined** into the prompt as a
  delimited block (so a text-only model still sees its content).

Media attachment is **caps-gated**: the server advertises whether its wired model
consumes images/audio (the `image`/`audio` server capabilities). Mentioning an image
file to a text-only model is a **loud refusal** — the submit is rejected with an
inline `attach: …` error, the input is kept, and nothing is sent (never a silent
drop). Client-side size limits (10 MiB per file, 20 MiB and 16 parts per prompt)
mirror the server's domain caps and fail fast before opening a stream; the server
re-validates every part regardless.

### `ctrl+v` — paste a clipboard image

`ctrl+v` reads the **OS clipboard**. The contract is **image-first, text-fallback**:

- a clipboard **image** is staged as an inline attachment and an `[Image #N]` marker
  is inserted into the prompt (cap-gated on `image` — on a text-only model the image
  is refused with a status line and nothing is staged);
- otherwise the clipboard **text** is inserted into the prompt (always — text is not
  cap-gated, so `ctrl+v` still pastes text on an image-incapable model).

On submit, every surviving `[Image #N]` marker becomes a media part (ascending by
`N`, in display order), the marker is **stripped** from the sent text, and the part
rides the same `Prompt.parts` send path as an `@`-mention attachment — there is no
second send path. Deleting a marker from the input before sending drops that
attachment. `N` is monotonic and never reused (a delete leaves a numbering gap, by
design); `/clear` drops any staged-but-unsent attachments.

Dragging an image file onto the terminal usually arrives as a **pasted path**; a
single media-file path is staged the same way (an `[Image #N]` marker), and anything
that is not a stageable media file stays literal pasted text.

Clipboard reads shell out to a platform tool — **wl-clipboard** (`wl-paste`, Wayland),
**xclip** (X11), **pbpaste**/**pngpaste** (macOS), or **PowerShell** (Windows). With
none installed, `ctrl+v` reports an install hint. macOS caveat: `pngpaste` reads the
`«class PNGf»` pasteboard flavour, so an image copied from a **Chromium/Electron** app
(which uses the `public.png` flavour) may not paste as an image and falls back to text.

## Keys

| Key | Action |
|---|---|
| `enter` (idle) | send the prompt |
| `enter` (while a run streams) | **queue a follow-up** (staged, sent when the turn ends) |
| `shift+enter` (or `ctrl+j`) | newline in the input |
| paste (bracketed) | insert clipboard text into the prompt; a single pasted **media-file path** is staged as an attachment instead (ignored while an overlay/modal is open) |
| `ctrl+v` | read the OS clipboard — a clipboard **image** stages as an `[Image #N]` attachment (when supported), else paste clipboard **text** (see below) |
| `esc` (while a run streams) | clear staged input → else clear the queue → else cancel the in-flight run (sends `Cancel`; waits for the terminal result) |
| `enter` (idle, **paused queue**, empty input) | resume — send the next staged follow-up |
| `esc` (idle, **paused queue**) | clear staged input → else clear the queue |
| `ctrl+c` | graceful quit (double-press): with a non-empty prompt the first press **clears the input**; on an empty prompt it **arms** the guard and shows a footer hint — press `ctrl+c` again within 3s to exit. Any other key disarms. The fatal (dead-connection) screen exits on a single press. |
| in the permission modal: `a`/`y`/`enter` | allow |
| in the permission modal: `d`/`n`/`esc` | deny |
| in the permission modal: `←`/`→`/`tab` | toggle the focused button |
| `pgup` / `pgdn` | scroll the conversation up / down |
| `home` / `end` | jump to the top / bottom of the conversation (`end` resumes auto-follow) |
| mouse wheel | scroll the conversation (**alt screen only**; see below) |
| `?` | help overlay (on an empty prompt) |
| `/` | slash-command palette (built-in `/clear`, `/help`; caps-gated `/mcp`, `/agents`, `/team`, `/skills`, `/soul`, `/usermodel`, `/models`; plus workspace commands) |
| `ctrl+a` | open the **live agent-team overlay** (the full roster + per-member focus of the most-recent team) — works **while idle and mid-run**; inert under a permission modal. Same surface as `/team`. |
| `@` | file-mention menu — complete a workspace path, then attach it on submit (see below) |

The `?` overlay enumerates the rest of the chords — `ctrl+v` (paste a clipboard
image), `ctrl+o`/`ctrl+r`/`ctrl+p` (MCP inventory / resources / prompts), `ctrl+a`
(live agent-team overlay, available idle **and** mid-run), `ctrl+t`
(expand/collapse details), and the scroll keys (`pgup`/`pgdn`, `home`/`end`, mouse
wheel) — and greys out any whose feature the connected server has not enabled
(driven by the server's relayed capabilities). When the server serves agent
definitions (`caps.agents`), it also notes that `/agents` browses the definition
inventory.

**Scrollback and auto-follow.** The conversation viewport **auto-follows** the
bottom (tails streaming output) until you scroll up — with `pgup`, `home`, or the
mouse wheel. While scrolled up the header shows a muted **`↑ NN%`** position cue,
streaming continues to render *in place* (a new delta no longer yanks the view to
the bottom), and auto-follow stays off. Scrolling back to the bottom — `pgdn` past
the end, `end`, or the wheel — re-pins the view and resumes auto-follow.

The **mouse wheel** is only active on the alternate screen (the default full-screen
TUI). With `--inline` / `--no-alt-screen` the terminal's own scrollback and
selection are left untouched (no mouse capture). On the alt screen, capturing the
mouse for wheel-scroll also grabs plain click-drag, so to make a **native text
selection** hold **Shift** (in iTerm2, **⌥ Option**) while dragging — that bypasses
the app's mouse grab. (Bubble Tea v2 has no wheel-only mouse mode, so this
Shift-bypass is how wheel-scroll and text selection coexist.)

### Type-while-running and queued follow-ups

The input stays **focused while a run streams**, so you can compose the next
request without waiting. Pressing `enter` mid-run **enqueues** the (trimmed,
non-empty) line rather than starting a second concurrent run — the queue is capped
at 16; an over-cap `enter` is rejected with a muted `queue full (16)` status and the
input is kept. A muted card above the input shows `⏳ N queued` with up to three
previews (`+K more` over that).

When the run ends on a **healthy** stop, the queue drains **one at a time, FIFO**: the
oldest staged line is submitted through the ordinary prompt path (so it reopens the
session server-side exactly like a manual follow-up), and that run's completion drives
the next. A healthy stop is one where the model was *done* or merely hit a *size
bound* — `end_turn` (and the empty reason), **plus** the per-run limits `max_turns`
and `max_tool_calls` (the run just ran out of budget; firing the next staged prompt
reopens it with a fresh budget, which is what a lined-up "continue" wants).

On a **non-healthy** stop the drain **pauses and keeps** the queue — an error, a user
cancel, `max_consecutive_failures`, or a stream close — so a broken, failing, or
deliberately-cancelled run never silently fires the backlog. The card switches from
the muted `⏳ N queued` to a louder `⏸ N queued · paused: <reason>` with the resume/
clear keys, so a held queue is never mistaken for a hang. From there (idle), `enter`
on an empty line **resumes** (sends the next staged line) and `esc` **clears** the
queue; sending a fresh prompt also clears the pause and lets the queue drain at that
run's clean end.

A built-in (`/clear`, `/help`) typed mid-run is enqueued like any other line and
dispatched **at drain time**, when the phase is idle and the built-in's idle-guard is
satisfied (so a queued `/clear` clears the transcript instead of sending a prompt).
`/clear` itself empties the queue along with the rest of the session-derived state.

Queueing is **running-only**: while a permission modal is open the modal keys own the
keyboard unchanged (no mid-approval queueing).

## Theming

Themes are pure data: a `Palette` of semantic colour slots (e.g. `accent`,
`error`, `mdHeading`, `synKeyword`) from which all lipgloss styles and the
glamour markdown/code style config are derived. Three themes ship built in:

- **aztec** (default): jade/turquoise + gold on obsidian, terracotta errors.
- **mono**: neutral greyscale with a blue accent.
- **solar**: a warm light-leaning (solarized-ish) variant.

### Custom themes

Drop a JSON file into one of these directories (increasing precedence):

1. `$XDG_CONFIG_HOME/mecatui/themes/` (or `~/.config/mecatui/themes/`)
2. `<cwd>/.mecatui/themes/`
3. the `--theme-dir` directory

Each file is `{ "name": "...", "palette": { ...slots... } }`. The palette is
**merged over the Aztec base**, so a partial theme only needs the slots it wants
to change:

```json
{
  "name": "midnight",
  "palette": {
    "accent": "#7C5CFF",
    "primary": "#5CC8FF",
    "error": "#FF5C7C"
  }
}
```

Select it with `--theme midnight` (or set `theme` / `MECATUI_THEME`).

## Architecture & testing

- `cmd/mecatui/client/` — touches `contracts/gen` + grpc: dial, `CreateSession`,
  the `Converse` stream wrapper (serialised sends), the reader goroutine, the
  `Event → tea.Msg` mapper, and the `IsReachable` health probe for auto mode.
- `cmd/mecatui/embed/` — hosts the embedded server: `embed.Start(ctx, app.Config)`
  builds the harness via `internal/app` and serves it over a UNIX socket. The only
  TUI package besides `client`/main that imports `internal/...` + grpc.
- `cmd/mecatui/ui/` — the Bubble Tea model/update/view + renderers. Imports
  `client` and `theme` only; never `contracts/gen` directly.
- `cmd/mecatui/theme/` — pure styling: palette, derived styles, glamour config,
  registry, JSON loading. No `contracts/gen`, no `ui`, no grpc.

**The emoji width-method invariant.** Assistant markdown is wrapped by glamour and
painted by Bubble Tea's differential renderer, and the two measure cell width with
DIFFERENT methods: glamour wraps on GraphemeWidth (a VS16 selector promotes a
cluster to width 2), while the renderer paints on WcWidth on any terminal that does
not confirm DEC mode 2027 (Apple Terminal, most SSH sessions). When the two
disagree on an emoji cluster, every cell to its right is offset and the line
scrambles ("mecatl" → "mec##atl"), persisting after the stream settles. `render.go`
normalises emoji presentation (`normalizeEmojiWidth`, strips VS16 + collapses any
residual divergent cluster) BEFORE glamour, so for every rendered line
`WcWidth == GraphemeWidth` — the two layers agree without relying on the terminal
upgrading the renderer. The invariant is guarded by
`TestMarkdownWidthMethodAgreement`. `trimTrailingSpaces` and the reserve-final-column
wrap are retained as harmless hygiene, not the fix.

**Streaming render coalescing.** Streamed assistant/reasoning deltas arrive far
faster than the eye can see, and a full conversation re-render per token would
re-run glamour on the live (growing) block every token — O(n²) over a turn. So a
delta only appends to the conversation and marks the view dirty (`m.viewDirty`); it
does NOT re-render. The first delta of a burst arms a single one-shot frame-cadence
tick (`renderTickMsg`, ~16ms ≈ one 60fps frame, guarded by `tickArmed` so a burst
schedules exactly one tick, not one per delta); the tick flushes the dirty view,
disarms, and re-arms only if more deltas arrived — so it idles to zero when the
stream goes quiet and never free-runs. Every turn/tool/result/error boundary still
force-flushes (via `afterEvent`/`endRun`, both of which call `refreshView`, which
clears `viewDirty`), so no flush depends on the tick: a dropped or late tick can
never lose the tail, and the final frame and event ordering are unchanged — only the
per-token re-render churn is coalesced. This does NOT touch the emoji
width-normalization path above.
The `renderer.mdRenders` counter (incremented only at the real `glamour` call site)
is the test seam: N coalesced deltas leave it unchanged, one flush bumps it by one
(`coalesce_test.go`).

Tests are fully offline and deterministic: the stream is driven from a scripted
fake behind the `Recv()` interface (no gRPC, no network), and whole-program /
View goldens are captured with teatest at a fixed terminal size. Refresh the
goldens with:

```sh
task test:golden     # go test ./cmd/mecatui/ui -update, then re-run
```
