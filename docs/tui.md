# mecatui — the terminal UI for mecatl

`mecatui` is a flashy, themeable terminal UI for the mecatl harness. It is a
**gRPC client** of a running `mecated` server: it creates a session, opens the
bidi `Converse` stream, renders the streamed events (glamour markdown for
assistant text, themed lipgloss cards for user prompts and tool I/O), shows a
thinking spinner and a status/usage footer, and resolves permission prompts
inline by sending `ResumeApproval` back on the same stream.

It is built on the Charm v2 stack (Bubble Tea / Lip Gloss / Bubbles / Glamour)
and is a pure client — it never imports any `internal/...` package and renders
solely from the proto `Event` envelope.

## Build

```sh
task build          # → bin/mecated, bin/mecademo, bin/mecatui
```

## Run

Start a server, then point the TUI at it:

```sh
# 1. start mecated (defaults to loopback gRPC 127.0.0.1:8080, no auth)
bin/mecated &

# 2. launch the TUI against it, with an absolute workspace
bin/mecatui --server 127.0.0.1:8080 --workspace "$PWD"
```

`--workspace` defaults to the current directory and is always resolved to an
absolute path (the server requires absolute). If `--server` is omitted it
defaults to `127.0.0.1:8080`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--server` | `127.0.0.1:8080` | mecated gRPC address (`host:port`) |
| `--workspace` | cwd | absolute session workspace root |
| `--mode` | `default` | permission posture: `default` \| `plan` \| `accept-edits` |
| `--theme` | `aztec` | theme name (also `MECATUI_THEME`) |
| `--theme-dir` | – | extra directory of `*.json` themes to load |
| `--auth-token` | – | bearer token (or `MECATL_AUTH_TOKEN`) |
| `--tls` | off | use TLS transport |
| `--tls-ca` | – | PEM CA bundle for server verification |
| `--insecure` | off | skip TLS verification (testing only) |
| `--list-themes` | – | print available themes and exit |

Loopback is unauthenticated plaintext by default, matching mecated's trust
model. For a non-loopback server, pass `--auth-token` (and `--tls` /
`--tls-ca` as the server requires).

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

- `cmd/mecatui/client/` — the only package touching `contracts/gen` + grpc:
  dial, `CreateSession`, the `Converse` stream wrapper (serialised sends), the
  reader goroutine, and the `Event → tea.Msg` mapper.
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
