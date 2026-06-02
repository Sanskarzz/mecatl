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
OPENAI_API_KEY=sk-... bin/mecatui --workspace "$PWD"

# Offline, no network — uses the canned mock provider:
bin/mecatui --mock --workspace "$PWD"
```

With no `--server`, mecatui runs in **auto** mode: it first probes the loopback
default `127.0.0.1:8080` and **reuses a `mecated` already running there**; only if
none answers does it host an **embedded** server itself (a UNIX socket in
`$XDG_RUNTIME_DIR`, torn down on exit). The embedded provider is OpenAI when
`OPENAI_API_KEY` is set, else the offline mock (`--mock`).

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
| `--model` | `gpt-5` | model id for the **embedded** server |
| `--openai-base-url` | – | OpenAI base URL override for the **embedded** server |
| `--mock` | off | **embedded** server: use the offline mock provider (no network) |
| `--no-bash` | off | **embedded** server: disable the Bash tool (shell-less) |
| `--memory-dir` | – (auto) | **embedded** server: per-project memory store dir; empty = a default under `$XDG_DATA_HOME/mecatui/memory` |
| `--no-memory` | off | **embedded** server: disable cross-session memory (Remember/Recall) |
| `--commands-dir` | – (auto) | **embedded** server: slash-command template dir; empty = the conventional `.mecatl/commands`, `.claude/commands` |
| `--no-commands` | off | **embedded** server: disable slash-command expansion |
| `--skills-dir` | – (auto) | **embedded** server: skill-unit dir (`<name>/SKILL.md`); empty = the conventional dirs (e.g. `.claude/skills`) |
| `--no-skills` | off | **embedded** server: disable skill discovery (the Skill tool) |

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
overlay reflects that. Pass `--no-skills` to disable discovery entirely, or
`--skills-dir` to scope it to a single vetted directory. Note the trust boundary:
a skill auto-activates from its always-in-context metadata, so a `SKILL.md` in a
workspace you didn't author (e.g. a cloned repo's `.claude/skills`) can steer the
model — use `--no-skills` for untrusted workspaces. Only read-only discovery is
wired; the writable SkillDraft self-improvement loop stays off.

**Slash commands are ON by default**, expanding `/<name>` inputs from the
conventional workspace dirs `.mecatl/commands` and `.claude/commands` (`<name>.md`
templates — the Claude Code convention). They're local, user-authored prompt
templates, so there's no network or trust cost (unlike MCP prompts, which stay off
with MCP). Pass `--no-commands` to disable expansion, or `--commands-dir` to point
at a different directory. When neither command dir exists the palette is simply
empty (the `?` overlay and footer reflect that honestly).

**Cross-session memory (Remember/Recall) is ON by default**, scoped per-project
under `~/.local/share/mecatui/memory/<path-slug>/` (or `$XDG_DATA_HOME/...` when
set), where `<path-slug>` is the absolute workspace path with `/` replaced by `-`
(e.g. `-var-home-ozz-dev-mecatl`) — deterministic, human-legible, and collision-free
across same-named checkouts. Pass `--no-memory` to disable it or `--memory-dir` to
relocate the store. Background memory consolidation (the "dream" distiller, which
spends tokens) stays **off** on the embedded server.

## Keys

| Key | Action |
|---|---|
| `enter` | send the prompt |
| `shift+enter` (or `ctrl+j`) | newline in the input |
| `esc` | cancel the in-flight run (sends `Cancel`; waits for the terminal result) |
| `ctrl+c` | quit |
| in the permission modal: `a`/`y`/`enter` | allow |
| in the permission modal: `d`/`n`/`esc` | deny |
| in the permission modal: `←`/`→`/`tab` | toggle the focused button |
| `pgup` / `pgdn` | scroll the conversation |
| `?` | help overlay (on an empty prompt) |

The `?` overlay enumerates the rest of the chords — `ctrl+o`/`ctrl+r`/`ctrl+p`
(MCP inventory / resources / prompts), `ctrl+a` (agent team), `ctrl+t`
(expand/collapse details) — and greys out any whose feature the connected server
has not enabled (driven by the server's relayed capabilities).

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

Tests are fully offline and deterministic: the stream is driven from a scripted
fake behind the `Recv()` interface (no gRPC, no network), and whole-program /
View goldens are captured with teatest at a fixed terminal size. Refresh the
goldens with:

```sh
task test:golden     # go test ./cmd/mecatui/ui -update, then re-run
```
