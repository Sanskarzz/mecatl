// Package ui is the Bubble Tea (Elm) layer of mecatui: the root Model state
// machine, its Update reducer, the View assembly, and the conversation/block
// renderers. It imports only the client and theme packages (plus charm libs and
// stdlib) — never contracts/gen or any internal/... package — so it renders
// purely from the plain msg structs the client layer translates proto Events
// into. All glamour rendering happens here on the single update goroutine.
package ui

import (
	"context"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// SessionCreator creates a server-side session and returns its id together with
// the server's advertised capabilities. *client.Client satisfies it (via the
// sessionAdapter); tests supply a fake. Keeping it an interface lets the ui be
// driven entirely offline. Capabilities is the proto-free relayed truth the ui
// stores for its honest discoverability affordances (Phase B); an older server
// yields the all-false zero value.
type SessionCreator interface {
	CreateSession(ctx context.Context) (string, client.Capabilities, error)
}

// Converser opens one Converse run as a *client.Stream. *client.Client satisfies
// it (its OpenConverse, wrapped to fix the mode/ctx); tests supply a fake that
// returns a Stream over a scripted Recver.
type Converser interface {
	OpenConverse(ctx context.Context) (*client.Stream, error)
}

// Deps are the ui's injected collaborators and presentation config. The ui
// imports client + theme only — never contracts/gen or any internal/... package;
// all proto contact happens behind Converser/SessionCreator.
type Deps struct {
	Session SessionCreator
	Conv    Converser
	MCP     client.MCP       // MCP/ToolHive inventory + resources/prompts; nil disables the overlay
	Cmds    client.Commander // slash-command discovery for the input palette; nil disables it
	Theme   theme.Theme

	// Display-only context for the header bar.
	Server    string
	Workspace string
	Mode      string
	Model     string

	// ContextWindow is the model's context-window size in tokens, used as the
	// denominator of the footer context meter. 0 means unknown (the meter then
	// shows just the current context size, no bar/percentage). Computed in main
	// from an explicit --context-window flag; never inferred from the model name.
	ContextWindow int64

	// Ctx is the program-level context; per-run stream contexts derive from it.
	Ctx context.Context //nolint:containedctx // stored to parent per-run stream cancels

	// NoAltScreen disables the alternate screen buffer, rendering inline in the
	// terminal's normal buffer. Default false (full-screen TUI on the alt screen).
	// Set true by the --no-alt-screen/--inline flag — a first-class user opt-out
	// for streaming the session into native scrollback (so it stays
	// searchable/scrollable after exit) — and by golden tests so the final frame
	// persists in the captured output instead of being cleared on exit.
	NoAltScreen bool
}

// phase is the model's coarse state machine.
type phase int

const (
	phaseConnecting       phase = iota // awaiting CreateSession
	phaseIdle                          // ready for a prompt
	phaseRunning                       // a Converse run is streaming
	phaseAwaitingApproval              // a permission modal is open
	phaseFatal                         // connect/fatal error; input disabled
)

// Model is the root Elm model. It owns the conversation, the bubbles widgets, the
// renderer (glamour cache), the active run stream, and the per-run cancel func.
type Model struct {
	deps Deps
	keys keyMap
	rend *renderer

	phase     phase
	sessionID string
	statusMsg string
	fatalErr  string

	width  int
	height int

	conv  conversation
	vp    viewport.Model
	ta    textarea.Model
	sp    spinner.Model
	stuck bool // viewport pinned to bottom

	activeTool   string         // tool name in flight, shown beside the spinner
	toolProgress string         // transient progress line for the in-flight tool (cleared on result/turn boundary)
	ask          pendingAsk     // current permission modal (when phaseAwaitingApproval)
	mcp          mcpState       // MCP overlay state (view==mcpNone when closed)
	palette      paletteState   // slash-command palette (open when the input starts with "/")
	agents       agentsState    // agent-team overlay state (view==agentsNone when closed)
	showHelp     bool           // the "?" keys-&-features overlay is open (caps-driven; see help.go)
	stream       *client.Stream // current run's stream
	cancelRun    context.CancelFunc

	// caps is the connected server's advertised capabilities, delivered once on
	// SessionReadyMsg. It drives the honest discoverability affordances (which
	// chords the help overlay annotates as available, and whether an empty
	// MCP/commands box reads "not enabled" vs "none configured"). Zero value
	// (all-false) until connect and for an older server. STORED, UNRENDERED in
	// Phase A — Phase B consumes it.
	caps client.Capabilities

	// usage accumulates across the session for the footer.
	usage client.Usage

	// contextTokens is the latest turn's prompt size (its Usage.InputTokens,
	// which already includes cache-served tokens) — i.e. the CURRENT context
	// size, the numerator of the footer context meter. Distinct from usage,
	// which is the cumulative session total.
	contextTokens int64

	// expandTools toggles all tool-result bodies (and Edit/Write diffs) between
	// the line-capped view and the full view. Flipped by ctrl+t.
	expandTools bool

	// filesChanged is the de-duplicated, insertion-ordered set of workspace paths
	// touched by file-MUTATING tool calls (Edit/Write) this session, derived
	// purely from observed tool.call events (no proto/server change). filesSeen is
	// the membership set guarding the order-preserving slice against duplicates.
	// Surfaced as a muted "Δ N files" header indicator, with the list folded into
	// the ctrl+t details expansion.
	filesChanged []string
	filesSeen    map[string]struct{}

	// streamCh is the current run's reader channel; WaitForMsg drains it.
	streamCh chan tea.Msg
}

// New builds the root model from deps. It wires the widgets but does not connect;
// Init kicks off CreateSession.
func New(deps Deps) Model {
	if deps.Ctx == nil {
		deps.Ctx = context.Background()
	}
	th := deps.Theme

	ta := textarea.New()
	ta.Placeholder = "Ask mecatl to do something…  (enter to send · shift+enter for newline · ? for help)"
	ta.SetHeight(3)
	ta.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(th.Style("spinner")))

	vp := viewport.New()

	return Model{
		deps:  deps,
		keys:  defaultKeys(),
		rend:  newRenderer(th),
		phase: phaseConnecting,
		ta:    ta,
		sp:    sp,
		vp:    vp,
		stuck: true,
	}
}

// recordFileChange folds a workspace path touched by a file-mutating tool into
// the session's changed-files set, preserving first-seen order and ignoring
// duplicates. Non-mutating / unrecognised tools never reach here (the caller
// gates on mutatedPath). Lazily initialises the membership set so a zero Model
// needs no constructor wiring.
func (m *Model) recordFileChange(path string) {
	if path == "" {
		return
	}
	if m.filesSeen == nil {
		m.filesSeen = make(map[string]struct{})
	}
	if _, ok := m.filesSeen[path]; ok {
		return
	}
	m.filesSeen[path] = struct{}{}
	m.filesChanged = append(m.filesChanged, path)
}

// Init starts the spinner and kicks off the async CreateSession.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, m.createSessionCmd())
}
