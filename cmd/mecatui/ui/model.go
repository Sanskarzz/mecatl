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
	CreateSession(ctx context.Context, sel client.ModelSelection) (string, client.Capabilities, error)
}

// SelectionStore persists + loads the client-side model selection (last-used). It
// is satisfied by a main-owned concrete type backed by an XDG state file; nil
// cleanly disables persistence (the active selection then lives only for the run).
// The ui touches no os/xdg itself — persistence is composition-side, like
// SessionCreator. Save is given the workspace so the store can key per-workspace.
type SelectionStore interface {
	Save(workspace string, sel client.ModelSelection) error
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
	Session   SessionCreator
	Conv      Converser
	MCP       client.MCP             // MCP/ToolHive inventory + resources/prompts; nil disables the overlay
	Cmds      client.Commander       // slash-command discovery for the input palette; nil disables it
	Skills    client.SkillLister     // skills-inventory discovery for the /skills panel; nil disables it
	Agents    client.AgentLister     // agent-definition discovery for the /agents panel; nil disables it
	Soul      client.SoulFetcher     // soul (persona) inspection for the /soul panel; nil disables it
	UserModel client.UserModelLister // user-model inspection for the /usermodel panel; nil disables it
	Models    client.ModelLister     // selectable-model discovery for the /models picker; nil disables it
	// SelectionStore persists the picked model (last-used). nil disables persistence
	// (the pick still applies to the next create this run, just isn't remembered).
	SelectionStore SelectionStore
	// InitialModel is the persisted selection loaded at launch (composition-side,
	// from the state file). The picker seeds its active selection from it (the ●
	// marker) and the startup CreateSession carries it — AFTER the connect-time
	// ListModels reconcile clears it if its provider is no longer available.
	InitialModel client.ModelSelection
	// Clipboard reads the OS clipboard for ctrl+v paste (image-first, text-fallback).
	// nil cleanly disables ctrl+v image paste (same convention as nil MCP/Cmds);
	// main.go populates it with client.NewClipboard().
	Clipboard client.Clipboard
	Theme     theme.Theme

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

	// onPhase is a test-only observer (nil in production, unexported so no external
	// caller can set it) invoked by Update on the SINGLE update goroutine after each
	// reduced message, with the model's current phase. The teatest cases use it to
	// sequence input on the program's ACTUAL reducer progress instead of on rendered
	// output: under `task test`'s parallel `go test -race ./...` the Bubble Tea 60fps
	// flush ticker is CPU-starved and the captured output stalls for seconds, so a
	// WaitFor(tm.Output()) deadline fires before any frame is flushed (the historical
	// "~1/3 -race flake", in truth far worse under load). The reducer goroutine keeps
	// getting scheduled, so observing it directly is starvation-robust.
	//
	// Why the cleaner alternatives don't cover what this serves — a future maintainer
	// may want to drop it, so the tradeoff is recorded honestly:
	//   - tm.FinalModel() exposes only the FINAL model after the program exits. It
	//     cannot gate an INTERMEDIATE step mid-run — e.g. "the reducer has reached
	//     phaseAwaitingApproval, now send the approval keypress". The approval
	//     round-trip must be driven WHILE the program runs, before quit.
	//   - The fake-side reachedGate signal (fakeRecver) fires when the permission.ask
	//     is YIELDED to the ReadLoop — the input side of the gate. It does NOT observe
	//     the reducer actually entering phaseAwaitingApproval, nor the reducer
	//     RETURNING to idle after the terminal result (waitRunComplete's
	//     running→idle transition) — both reducer-side facts the fake cannot see
	//     because the fake has no handle on the model. onPhase is the minimal seam
	//     that surfaces exactly those reducer transitions.
	// (An all-fake-side scheme that also signals run-completion would remove this
	// field; that rework is deferred. For now: nil ⇒ zero cost, zero behaviour change.)
	onPhase func(phase)
}

// maxQueued caps the number of follow-up prompts that may be staged while a run
// streams. A further enqueue over the cap is rejected with a muted "queue full"
// status and the input is kept, so a typo'd burst can't grow the queue without
// bound. The drain is one-at-a-time FIFO (see drainQueue), so the cap is the only
// backpressure the queue needs.
const maxQueued = 16

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

	conv conversation
	vp   viewport.Model
	ta   textarea.Model
	sp   spinner.Model
	// stuck is true while the viewport auto-follows the bottom (tails streaming
	// output). It is no longer hardcoded: syncStuck re-derives it from
	// m.vp.AtBottom() after every scroll/wheel/nav so a scroll-up unsticks (and
	// survives streaming — refreshView only re-pins to bottom when stuck) and
	// scrolling/jumping back to the bottom re-sticks (auto-follow resumes). The
	// initial value is true because an empty conversation is already at-bottom.
	stuck bool

	// viewDirty is set when a streamed delta mutated the conversation but
	// refreshView has not yet re-rendered it into the viewport. A one-shot
	// frame-cadence tick (renderTickCmd) flushes it at most once per frame,
	// coalescing the per-token re-renders into one render per visible frame.
	// refreshView clears it, so "rendered ⟺ not dirty" is invariant.
	viewDirty bool

	// tickArmed is true while a renderTickMsg is in flight (a flush tick has been
	// scheduled but not yet handled). A delta arms the tick ONLY when none is
	// pending, and the tick disarms itself when handled — so the ticker is a single
	// self-disarming one-shot driven by delta activity, not a free-running 60fps
	// loop. That bounds the extra render churn to actual streaming bursts (idle gaps
	// and the post-run tail cost no ticks), keeping the teatest frame cadence — and
	// thus its known ~1/3 -race flake — no worse than before.
	tickArmed bool

	activeTool   string         // tool name in flight, shown beside the spinner
	toolProgress string         // transient progress line for the in-flight tool (cleared on result/turn boundary)
	ask          pendingAsk     // current permission modal (when phaseAwaitingApproval)
	mcp          mcpState       // MCP overlay state (view==mcpNone when closed)
	skills       skillsState    // skills-inventory overlay state (view==skillsNone when closed)
	palette      paletteState   // slash-command palette (open when the input starts with "/")
	mention      mentionState   // @-file-mention completion menu (open when the trailing word is an "@token"); mutually exclusive with palette
	queued       []string       // follow-up prompts staged while a run streams; drained FIFO on a clean stop (see drainQueue)
	queuePaused  string         // non-empty when a run ended on a non-clean stop with a non-empty queue: the stop reason holding the queue (see drainQueue/renderQueue)
	team         teamState      // live agent-team overlay state (view==teamNone when closed)
	agentsInv    agentsInvState // agent-definition inventory overlay state (view==agentsInvNone when closed)
	soul         soulState      // soul (persona) inspection overlay state (view==soulNone when closed)
	userModel    userModelState // user-model inspection overlay state (view==userModelNone when closed)
	models       modelsState    // /models picker overlay state (view==modelsNone when closed)
	// activeModel is the currently-selected (provider, model) the NEXT CreateSession
	// will carry (apply-on-next-create). Seeded from Deps.InitialModel, updated by the
	// picker, and reconciled-to-default at connect when its provider is unavailable. It
	// is the SOURCE of truth for the create selection; m.models.active mirrors it for
	// the picker's ● marker. The header model display reads from it once non-zero.
	activeModel client.ModelSelection
	showHelp    bool           // the "?" keys-&-features overlay is open (caps-driven; see help.go)
	stream      *client.Stream // current run's stream
	cancelRun   context.CancelFunc

	// quitArmed is true after a first ctrl+c on an empty prompt: a second ctrl+c
	// within quitArmWindow then quits (Claude Code's "press again to exit"
	// convention). Any other key disarms it, and a timed quitDisarmMsg disarms it
	// when the window lapses. quitArmGen is the monotonic arm generation: the
	// disarm tick carries the gen it was armed with, so a stale tick (the guard was
	// disarmed and re-armed in between) is ignored. NOT reset in resetSession — the
	// quit guard is transport/compose state, not session-derived transcript state.
	quitArmed  bool
	quitArmGen int

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

	// streamGen is the monotonic generation of the CURRENT run's stream. Every
	// reader command (waitCmd) tags the message it delivers with the generation that
	// was current when it was armed; the reducer drops any stream message whose
	// generation no longer matches (see Update). submitPrompt bumps it when it opens a
	// new run and endRun bumps it when it tears one down, so a reader left bound to an
	// ABANDONED stream channel (e.g. one leaked across a queue-drain) can never route
	// its messages — a stale StreamClosed/StreamErr can't cancel the new run, and a
	// stale event can't re-arm a reader on the new channel. This is the structural
	// backstop for the hand-maintained "exactly one reader per run" fan-in invariant:
	// even if a future handler leaks an extra reader, its messages become inert at the
	// next run boundary regardless of how many readers leaked.
	streamGen uint64

	// stagedMedia holds clipboard/pasted-path image attachments not yet sent,
	// keyed by their literal "[Image #N]" marker (which also sits in the textarea
	// text). nextMediaN is the monotonic marker counter. The design is
	// no-live-renumber + reconcile-at-submit: a marker's N is assigned once and
	// never reused (deleting a marker leaves a numbering gap — fine, documented),
	// and submitPrompt reconciles by which markers still survive in the sent text
	// (survivingMarkers), ordering the parts ascending by N. This keeps the paste
	// handler O(1) and avoids renumbering every staged marker on each edit.
	stagedMedia map[string]stagedAttachment
	nextMediaN  int

	// sel is the in-app text-selection state (mouse-drag select + copy over the
	// conversation viewport). Zero value = inactive. Its coordinates are LOGICAL
	// content positions (line index + grapheme column into the ansi-stripped line),
	// so the highlight survives scrolling and a streaming re-render — the byte
	// ranges are recomputed from the CURRENT content each frame (see
	// applySelectionHighlight / refreshView). Active only on the alt screen; an
	// overlay/modal/help blocks a new selection and clears an active one.
	sel selection
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
	// In-app text-selection highlight style. The viewport re-applies it on every
	// render (it survives scroll/stream), and SetContent clears the ranges — so the
	// ranges are re-applied after each refreshView (applySelectionHighlight) while a
	// selection is active. Reverse video is theme-independent and ANSI-strips cleanly.
	vp.HighlightStyle = th.Style("selection")

	return Model{
		deps:  deps,
		keys:  defaultKeys(),
		rend:  newRenderer(th),
		phase: phaseConnecting,
		ta:    ta,
		sp:    sp,
		vp:    vp,
		stuck: true,
		// Seed the active selection from the persisted last-used (composition loads it
		// from the state file). The connect-time ListModels reconcile clears it to the
		// server default if its provider is no longer available, BEFORE the create that
		// carries it (so a removed key never hard-fails the connect with InvalidArgument).
		activeModel: deps.InitialModel,
		models:      modelsState{active: deps.InitialModel},
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

// resetSession is the single seam that owns "the session-derived state of the
// Model": the conversation transcript plus everything accumulated FROM the stream
// over a session (changed-files set, cumulative usage, current context size, and
// the in-flight tool affordances). It is co-located with the field declarations
// it zeroes (see the Model struct above) so that ANY future session-derived field
// added there has an obvious, single place to be reset — keeping /clear honest
// without each call site re-listing fields.
//
// It deliberately does NOT touch the per-RUN transport teardown (stream /
// streamCh / cancelRun / phase / textarea focus) — that is endRun's concern and
// its semantics are relied on by the idle-guard. The only overlap is the in-flight
// tool affordances (activeTool/toolProgress), which are genuinely both
// "session-derived display state" and "cleared at run end"; resetSession owns
// them here, endRun continues to clear activeTool on its own teardown path. The
// caller is responsible for re-rendering (refreshView) after calling this.
//
// It also drops any staged follow-up prompts (queued): /clear wipes the
// session-derived state, and a queue of as-yet-unsent follow-ups is part of that
// state — leaving them to drain into a freshly-cleared transcript would surprise.
func (m Model) resetSession() Model {
	m.conv = conversation{}
	// An empty conversation is at-bottom by definition, so auto-follow must be
	// re-armed: without this a /clear issued while scrolled up (stuck=false) would
	// strand stuck false, and refreshView (re-pins only if stuck) would silently
	// fail to tail the NEXT run's streaming deltas until the user manually hit End.
	m.stuck = true
	m.filesChanged = nil
	m.filesSeen = nil
	m.usage = client.Usage{}
	m.contextTokens = 0
	m.activeTool = ""
	m.toolProgress = ""
	m.queued = nil
	m.queuePaused = ""
	// Drop staged-but-unsent media attachments: /clear wipes the session-derived
	// state, and pasted-but-unsent images are part of that compose state.
	m.stagedMedia = nil
	m.nextMediaN = 0
	// Drop any active text selection: /clear rebuilds the transcript, so a selection
	// anchored into the old content is stale. The caller's refreshView re-renders
	// without re-applying it (sel is now inactive), clearing the highlight too.
	m.sel = selection{}
	return m
}

// Init starts the spinner and kicks off connect.
//
// Connect SEQUENCING (§4 key-removed safety): when a model lister is wired, it
// fetches ListModels FIRST and lets the connecting-phase ModelsMsg reconcile the
// persisted selection against availability BEFORE firing CreateSession — so the
// startup create carries only a validated selection and a removed provider key can
// never hard-fail the connect with InvalidArgument. With no lister wired (old
// server / persistence off) it fires CreateSession directly (the historical path,
// with an empty selection).
func (m Model) Init() tea.Cmd {
	if m.deps.Models != nil {
		return tea.Batch(m.sp.Tick, client.ListModelsCmd(m.deps.Ctx, m.deps.Models))
	}
	// No-lister / old-server path: with no model lister wired there is nothing to
	// reconcile, so fire CreateSession directly (with the empty selection) — do NOT
	// wait on a ListModels that will never arrive, which would strand at "connecting…".
	return tea.Batch(m.sp.Tick, m.createSessionCmd())
}
