package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// maxToolResultLines caps how many lines of a tool result are shown inline; the
// rest collapse into a "+N more lines" affordance so a giant Read result doesn't
// drown the scrollback.
const maxToolResultLines = 12

// maxDiffLines caps how many lines of each diff side (Edit old/new, Write
// content) show inline when collapsed; ctrl+t expands to the full diff.
const maxDiffLines = 12

// renderer turns conversation blocks into the viewport string. It owns the
// glamour TermRenderer cache (keyed by wrap width) and the active theme. glamour
// is NOT thread-safe, so renderer is only ever touched from the Bubble Tea
// update goroutine — never from the stream reader. The mutex guards the cache map
// against the (currently single-goroutine) access defensively and documents the
// invariant; it does not make glamour itself concurrency-safe.
type renderer struct {
	th    theme.Theme
	width int

	mu    sync.Mutex
	cache map[int]*glamour.TermRenderer
}

// newRenderer builds a renderer for a theme.
func newRenderer(th theme.Theme) *renderer {
	return &renderer{th: th, cache: map[int]*glamour.TermRenderer{}}
}

// setWidth records the current wrap width. Width changes are handled by the
// cache key, so no explicit invalidation is needed.
func (r *renderer) setWidth(w int) { r.width = w }

// markdown renders src to ANSI through a width-cached glamour renderer themed by
// the active theme. On any glamour error it falls back to the raw text so the
// stream is never lost. MUST be called only on the update goroutine.
func (r *renderer) markdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	w := r.width
	if w <= 0 {
		w = 80
	}
	r.mu.Lock()
	tr, ok := r.cache[w]
	if !ok {
		built, err := glamour.NewTermRenderer(
			glamour.WithStyles(r.th.GlamourStyle()),
			glamour.WithWordWrap(w),
		)
		if err != nil {
			r.mu.Unlock()
			return src
		}
		tr = built
		r.cache[w] = tr
	}
	r.mu.Unlock()

	out, err := tr.Render(src)
	if err != nil {
		return src
	}
	return strings.TrimRight(out, "\n")
}

// renderConversation joins every block into the viewport content string. expand
// is the global tool-output toggle (ctrl+t): when true, tool result bodies and
// Edit/Write diffs render in full instead of line-capped.
func (r *renderer) renderConversation(c *conversation, expand bool) string {
	var b strings.Builder
	for i := range c.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(r.renderBlock(&c.blocks[i], expand))
		b.WriteString("\n")
	}
	return b.String()
}

// renderBlock renders one block per its kind. Assistant text goes through
// glamour; everything else is plain themed lipgloss.
func (r *renderer) renderBlock(b *block, expand bool) string {
	switch b.kind {
	case blockUser:
		label := r.th.Style("userLabel").Render("you")
		body := r.th.Style("userBlock").Render(sanitizeTerminal(b.raw))
		return label + "\n" + body
	case blockAssistant:
		// Assistant text is rendered through glamour, which neutralises escape
		// sequences itself — do NOT sanitize here or markdown breaks. The turn's
		// reasoning summary (if any) renders dim and collapsed ABOVE the answer.
		label := r.th.Style("assistantLabel").Render("mecatl")
		out := label
		if reasoning := r.renderReasoning(b, expand); reasoning != "" {
			out += "\n" + reasoning
		}
		return out + "\n" + r.markdown(b.raw)
	case blockTool:
		return r.renderTool(b, expand)
	case blockNotice:
		return r.th.Style("muted").Render("• " + sanitizeTerminal(b.raw))
	case blockHook:
		return r.renderHook(b)
	case blockTurnStat:
		return r.th.Style("muted").Render(sanitizeTerminal(b.raw))
	case blockError:
		return r.th.Style("errorText").Render("✗ " + sanitizeTerminal(b.raw))
	default:
		return sanitizeTerminal(b.raw)
	}
}

// maxReasoningLines caps how many lines of the reasoning summary show when the
// global details toggle (ctrl+t) is on; the rest collapse with a neutral
// "…(truncated)" tail so a long chain-of-thought never dominates the scrollback
// even when expanded.
const maxReasoningLines = 24

// reasoningCaveat is the dim one-line disclaimer prepended to the EXPANDED
// reasoning. It signals the prose is a lossy summary, not the model's actual
// process — streamed chain-of-thought is often unfaithful and drives
// over-reliance, so it must never read as ground truth.
const reasoningCaveat = "— summary of the model's reasoning; may not reflect its actual process"

// renderReasoning renders the dim, collapsed-by-default reasoning summary that
// belongs to an assistant block. It returns "" when the block carries no
// reasoning. Collapsed (the default) it is a single dim header: while reasoning
// is still streaming and no answer text has begun it reads "reasoning…" (a live
// "the model is working" affordance); otherwise it is the static
// "reasoning summary · N lines · ctrl+t expand". When the global details toggle
// (expand) is on, a dim caveat plus the full summary text are shown, line-capped
// so they cannot drown the answer. Streamed reasoning is never a trust anchor:
// hidden unless explicitly asked for, and clearly labelled as a lossy summary.
func (r *renderer) renderReasoning(b *block, expand bool) string {
	if b.reasoning == "" {
		return ""
	}
	style := r.th.Style("reasoning")
	text := sanitizeTerminal(strings.TrimRight(b.reasoning, "\n"))
	n := lineCount(text)
	if !expand {
		if b.reasoningStreaming {
			return style.Render("reasoning…")
		}
		return style.Render("reasoning summary · " + plural(n, "line") + " · ctrl+t expand")
	}
	header := style.Render("reasoning summary · " + plural(n, "line") + " · ctrl+t collapse")
	body := truncateLinesTail(text, maxReasoningLines, "  …(truncated)")
	return header + "\n" + style.Render(reasoningCaveat) + "\n" + style.Render(body)
}

// plural formats a count with a noun, pluralising with a trailing "s" for any
// count other than 1 (e.g. 0 lines, 1 line, 3 lines).
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// renderHook renders a structured hook notice as a distinct one-liner: a hook
// glyph + the lifecycle phase (and the related tool, for per-tool phases) + the
// hook's message, with the OUTCOME driving colour and a leading severity glyph.
// A blocked hook (which can abort a run) renders in the error style with a "✗"
// so it is visually distinct from a benign informational/modified notice (a dim
// "•" hook glyph) — never indistinguishable from a compaction notice. All text
// is server-derived, so it is sanitized before reaching lipgloss.
func (r *renderer) renderHook(b *block) string {
	// Lead label: a phase tag, falling back to a generic "hook" when no phase.
	// Every server-derived field (phase, tool, message) is sanitized before it
	// reaches lipgloss — see the file's CWE-150 invariant.
	label := "hook"
	if b.hookPhase != "" {
		label = "hook " + sanitizeTerminal(b.hookPhase)
	}
	if b.hookTool != "" {
		label += " · " + sanitizeTerminal(b.hookTool)
	}

	switch b.hookDecision {
	case string(client.HookBlocked):
		// Blocked: error style + "✗", matching the error-block severity cue so an
		// aborting hook can't be mistaken for a benign notice. The decision VERB is
		// owned client-side ("blocked"), and the server Text rides as the trailing
		// reason only — a redundant leading phase/verb echo is stripped so the phase
		// appears exactly once (on the label).
		return r.th.Style("errorText").Render("✗ " + label + ": blocked" + hookReason(b.raw, b.hookPhase))
	case string(client.HookModified):
		// Modified: info-coloured "✎" — an action was rewritten, notable but benign.
		return r.th.Style("hookModified").Render("✎ " + label + ": modified" + hookReason(b.raw, b.hookPhase))
	default:
		// Info (the baseline): dim "•" hook notice — the server Text is the body.
		line := label
		if b.raw != "" {
			line += ": " + sanitizeTerminal(b.raw)
		}
		return r.th.Style("muted").Render("• " + line)
	}
}

// hookReason normalises a hook's server Text into a trailing " — <reason>" tail
// for the client-owned verb (blocked/modified), stripping a redundant leading
// phase/verb echo so the phase is never doubled. It drops boilerplate that adds
// nothing beyond the label+verb (e.g. "blocked by PreToolUse hook",
// "PreToolUse hook rewrote …") and otherwise appends the sanitized text as the
// reason. An empty/fully-redundant Text yields "" (label + verb stand alone).
func hookReason(raw, phase string) string {
	reason := strings.TrimSpace(stripPhaseEcho(raw, phase))
	if reason == "" {
		return ""
	}
	return " — " + sanitizeTerminal(reason)
}

// stripPhaseEcho removes a leading phase/verb echo from a hook message so the
// phase isn't repeated once on the label and again in the body. It folds away
// the loop's own boilerplate forms:
//
//	"blocked by <Phase> hook"            → ""        (pure echo)
//	"<Phase> hook rewrote tool arguments…" → "rewrote tool arguments…"
//	"<Phase> hook returned a malformed…"   → "returned a malformed…"
//
// A leading "<Phase> hook " or "<Phase> " prefix is trimmed; anything else is
// returned unchanged. Phase-free messages pass through verbatim.
func stripPhaseEcho(raw, phase string) string {
	s := strings.TrimSpace(raw)
	if phase == "" {
		return s
	}
	// The "blocked by <Phase> hook" form is a pure echo of label+verb → drop it.
	if strings.EqualFold(s, "blocked by "+phase+" hook") {
		return ""
	}
	// Trim a leading "<Phase> hook " or "<Phase> " prefix (case-insensitive).
	for _, prefix := range []string{phase + " hook ", phase + " "} {
		if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			return strings.TrimSpace(s[len(prefix):])
		}
	}
	return s
}

// renderTool renders a tool-call card: status glyph + name + body, and, once
// resolved, a truncated result body beneath it. For Edit/Write the args are
// shown as a colourised diff instead of raw JSON (falling back to pretty JSON if
// the args don't parse as the expected shape). expand removes the line cap on
// the result body and the diff.
func (r *renderer) renderTool(b *block, expand bool) string {
	var glyph string
	switch {
	case !b.resolved:
		glyph = r.th.Style("toolName").Render("…")
	case b.resultError:
		glyph = r.th.Style("toolErr").Render("✗")
	default:
		glyph = r.th.Style("toolOk").Render("✓")
	}

	head := glyph + " " + r.th.Style("toolName").Render(sanitizeTerminal(b.toolName))

	// A subagent (Task) card renders its REDACTED child activity in place of raw
	// JSON args: a goal title plus a live/expanded/resolved status region. The
	// child's interior (args, results, message text) is isolated by design and is
	// never shown — only metadata.
	if b.team {
		// A Team card renders its BOUNDED per-member lanes in place of raw JSON
		// args: a team header plus a live/expanded/resolved region. Member content
		// is server-bounded and never enters the parent conversation.
		if tm := r.renderTeam(b, expand); tm != "" {
			head += "\n" + tm
		}
	} else if b.subagent {
		if sub := r.renderSubagent(b, expand); sub != "" {
			head += "\n" + sub
		}
	} else if diff, ok := r.renderToolDiff(b.toolName, b.toolArgs, expand); ok {
		// Edit/Write render their change as a diff in place of the raw JSON args.
		if diff != "" {
			head += "\n" + diff
		}
	} else if args := prettyJSON(b.toolArgs); args != "" {
		head += "\n" + r.th.Style("toolArgs").Render(args)
	}

	if b.resolved {
		body := resultBody(b.resultBody, expand)
		if body != "" {
			style := r.th.Style("toolArgs")
			if b.resultError {
				style = r.th.Style("errorText")
			}
			head += "\n" + style.Render(body)
		}
	}

	card := r.th.Style("toolCard")
	if r.width > 4 {
		card = card.Width(r.width - 2)
	}
	return card.Render(head)
}

// renderSubagent renders a Task card's REDACTED subagent region. It has three
// states, per the agreed UX, and shows only metadata — never the child's interior
// (args/results/message text are isolated by design):
//
//   - LIVE collapsed (default, not resolved): a calm one-liner under the goal —
//     "subagent · ↑<in> ↓<out> · N tools · ctrl+t trace". No live current-tool
//     name, no elapsed clock: counts update as events arrive, no ticker.
//   - EXPANDED (ctrl+t, not resolved): a wrapped row of glyph+name chips
//     (✓/✗ per child tool), capped at maxSubagentTrace, under a muted
//     "args/results hidden" honesty note.
//   - RESOLVED: a single muted stat line —
//     "subagent · <dur> · ↑<in> ↓<out> · N tools · stop:<reason>".
//
// The goal title always leads (a muted line) so a card is self-contained and
// legible even with several concurrent subagents interleaved. All subagent-derived
// strings (goal, tool names) are terminal-sanitized.
func (r *renderer) renderSubagent(b *block, expand bool) string {
	muted := r.th.Style("muted")
	var out strings.Builder
	if b.subGoal != "" {
		out.WriteString(muted.Render("↳ " + sanitizeTerminal(b.subGoal)))
		out.WriteString("\n")
	}

	if b.subDone {
		out.WriteString(muted.Render(subagentResolvedLine(b)))
		return strings.TrimRight(out.String(), "\n")
	}

	if expand {
		out.WriteString(muted.Render("subagent · args/results hidden"))
		if chips := r.renderSubagentChips(b); chips != "" {
			out.WriteString("\n")
			out.WriteString(chips)
		}
		return strings.TrimRight(out.String(), "\n")
	}

	out.WriteString(muted.Render(subagentLiveLine(b)))
	return strings.TrimRight(out.String(), "\n")
}

// subagentLiveLine is the calm, monotonic collapsed status line: token totals and
// a running tool count, plus the ctrl+t trace affordance. No current-tool name and
// no elapsed clock, so it updates only as events arrive (no ticker).
func subagentLiveLine(b *block) string {
	return fmt.Sprintf("subagent · ↑%s ↓%s · %s · ctrl+t trace",
		humanizeTokens(b.subUsage.InputTokens),
		humanizeTokens(b.subUsage.OutputTokens),
		plural(b.subToolCount, "tool"))
}

// subagentResolvedLine is the muted one-line summary shown once the child run has
// finished: duration, token totals, final tool count, and the stop reason.
func subagentResolvedLine(b *block) string {
	return fmt.Sprintf("subagent · %s · ↑%s ↓%s · %s · stop:%s",
		humanizeDuration(b.subDurationMs),
		humanizeTokens(b.subUsage.InputTokens),
		humanizeTokens(b.subUsage.OutputTokens),
		plural(b.subToolCount, "tool"),
		subagentStopLabel(b.subStop))
}

// chipSep is the two-space gap between adjacent child-tool chips in the expanded
// trace row.
const chipSep = "  "

// renderSubagentChips renders the expanded child-tool trace as a wrapped row of
// glyph+name chips (✓ ok / ✗ error), using the same status glyphs as the tool
// card. Chips are packed greedily and wrapped BETWEEN chips at the card's content
// width (measured by visible width, so ANSI styling and the chip glyphs don't
// throw off the wrap), so a long trace never splits a chip mid-name. Names are
// sanitized. Returns "" for an empty trace.
func (r *renderer) renderSubagentChips(b *block) string {
	if len(b.subTrace) == 0 {
		return ""
	}
	okStyle := r.th.Style("toolOk")
	errStyle := r.th.Style("toolErr")
	nameStyle := r.th.Style("toolName")
	chips := make([]string, 0, len(b.subTrace))
	for _, c := range b.subTrace {
		glyph := okStyle.Render("✓")
		if c.isError {
			glyph = errStyle.Render("✗")
		}
		chips = append(chips, glyph+" "+nameStyle.Render(sanitizeTerminal(c.name)))
	}
	return wrapChips(chips, r.chipContentWidth())
}

// chipContentWidth is the visible width available for the chip row inside the tool
// card, accounting for the card's border (2) and horizontal padding (2). It floors
// at a small positive value so a single chip per line is always attempted rather
// than degenerating when the width is unknown/tiny (r.width 0 → no wrap).
func (r *renderer) chipContentWidth() int {
	if r.width <= 4 {
		return 0 // width unknown/tiny: no wrapping (single row, as before)
	}
	w := r.width - 2 - 4 // card.Width(r.width-2) minus border(2)+padding(2)
	if w < 1 {
		w = 1
	}
	return w
}

// wrapChips packs already-rendered chips into rows separated by chipSep, breaking
// to a new line BETWEEN chips when the next chip would overflow width (measured by
// visible width via lipgloss.Width, which ignores ANSI). A width <= 0 disables
// wrapping (all chips on one row). A chip wider than width still gets its own row
// rather than being split.
func wrapChips(chips []string, width int) string {
	if len(chips) == 0 {
		return ""
	}
	if width <= 0 {
		return strings.Join(chips, chipSep)
	}
	sepW := lipgloss.Width(chipSep)
	var b strings.Builder
	lineW := 0
	for i, chip := range chips {
		cw := lipgloss.Width(chip)
		switch {
		case i == 0:
			b.WriteString(chip)
			lineW = cw
		case lineW+sepW+cw > width:
			b.WriteString("\n")
			b.WriteString(chip)
			lineW = cw
		default:
			b.WriteString(chipSep)
			b.WriteString(chip)
			lineW += sepW + cw
		}
	}
	return b.String()
}

// maxTeamMessageLen caps how many runes of a member's forwarded message line show
// in the expanded lane trace; the server already bounds previews, this is a
// belt-and-braces clamp so one verbose member can't dominate the card.
const maxTeamMessageLen = 200

// maxTeamDetailLen caps how many runes of a tool chip's arg/result preview show
// next to it in the expanded trace. Server-bounded already; this keeps a single
// chip line scannable.
const maxTeamDetailLen = 80

// maxTeamLanes caps how many member lanes render inline on the card. A larger
// roster collapses the overflow into a "· +K more" roll-up line so a big team can
// never grow the card without limit (a DoS-by-output guard) and stays legible.
// The remaining members are not lost — they live in the conversation block and a
// future ctrl+a overlay can surface them all.
const maxTeamLanes = 6

// maxTeamNameWidth caps the column width member names are padded to for the
// collapsed lane lines, so the "· <state> · ↑in ↓out" columns line up without one
// very long name blowing out the gutter.
const maxTeamNameWidth = 16

// teamGlyph is the per-member state glyph (glyph-not-colour-only): a hollow "○"
// for a member that has finished its terminal result, a filled "◆" for one still
// working.
func teamGlyph(ln *teamLane) string {
	if ln.done {
		return "○"
	}
	return "◆"
}

// teamLaneOrder returns lane indices in render order: the lead member(s) first,
// then the rest in roster (arrival) order. It is a stable sort over an index slice
// so teamLanes itself is never reordered (event routing stays by name). The lead
// is thus always anchored at the top regardless of the order the server sent the
// roster.
func teamLaneOrder(lanes []teamLane) []int {
	order := make([]int, len(lanes))
	for i := range lanes {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return lanes[order[a]].lead && !lanes[order[b]].lead
	})
	return order
}

// renderTeam renders a Team card's BOUNDED per-member region. It has three states,
// mirroring the subagent card's live/expanded/resolved language, and shows only
// server-bounded member content (never the raw member transcript):
//
//   - LIVE collapsed (default, team not ended): a team header ("N members") plus a
//     calm one-line-per-member lane (lead first, columns aligned) —
//     "◆ <name>[lead] · <current tool or state>… · ↑<in> ↓<out>". Counts are
//     monotonic and update only as events arrive (no ticker), so a lane never
//     flickers; an active member's state carries a trailing "…" heartbeat. At most
//     maxTeamLanes lanes render, with a "· +K more" roll-up for the rest.
//   - EXPANDED (ctrl+t, same toggle): per member, the capped lane trace — message
//     lines (clamped) and tool chips (✓/✗ name) with their bounded arg/result
//     preview — separated by a blank line between members so boundaries are clear.
//   - RESOLVED (team ended): a muted stat line
//     "team · <rounds> rounds · ↑<in> ↓<out> · stop:<reason>". The Team tool's
//     joined summary renders below via the normal result body path.
//
// All member-derived text (names, message lines, tool names, previews) is
// terminal-sanitized before it reaches lipgloss.
func (r *renderer) renderTeam(b *block, expand bool) string {
	muted := r.th.Style("muted")
	var out strings.Builder

	if b.teamDone {
		out.WriteString(muted.Render(teamResolvedLine(b)))
		return out.String()
	}

	out.WriteString(muted.Render(teamHeader(b, expand)))

	order := teamLaneOrder(b.teamLanes)
	shown := order
	if len(shown) > maxTeamLanes {
		shown = order[:maxTeamLanes]
	}
	nameW := teamNameWidth(b.teamLanes, shown)
	for n, idx := range shown {
		ln := &b.teamLanes[idx]
		if expand && n > 0 {
			// A blank line between members' blocks so boundaries read clearly at 3+.
			out.WriteString("\n")
		}
		out.WriteString("\n")
		out.WriteString(muted.Render(teamLaneLine(ln, nameW)))
		if expand {
			if tr := r.renderTeamTrace(ln); tr != "" {
				out.WriteString("\n")
				out.WriteString(tr)
			}
		}
	}
	if extra := len(order) - len(shown); extra > 0 {
		out.WriteString("\n")
		out.WriteString(muted.Render(fmt.Sprintf("  · +%d more", extra)))
	}
	return out.String()
}

// teamHeader is the muted lead line summarising the team's shape: the member count
// and the ctrl+t affordance, whose verb tracks the toggle (trace when collapsed,
// collapse when expanded). The round count is carried only on team.end, so it is
// shown on the resolved line rather than fabricated live.
func teamHeader(b *block, expand bool) string {
	verb := "ctrl+t trace"
	if expand {
		verb = "ctrl+t collapse"
	}
	return "team · " + plural(len(b.teamLanes), "member") + " · " + verb
}

// teamNameWidth is the column width member BARE names are padded to on the
// collapsed lane lines: the longest shown bare name, capped at maxTeamNameWidth,
// so the state/usage columns line up across members. The "[lead]" tag is appended
// AFTER this padded column (never truncated away), so the lead is always
// unambiguous even when its name is long.
func teamNameWidth(lanes []teamLane, shown []int) int {
	w := 0
	for _, idx := range shown {
		if n := len([]rune(truncate(sanitizeTerminal(lanes[idx].name), maxTeamNameWidth))); n > w {
			w = n
		}
	}
	return w
}

// teamLaneLine is one member's calm, monotonic collapsed status line: a state
// glyph, a persistent mutating cue, the bare member name (truncated + column-
// padded) with the "[lead]" tag appended after the column, then the current tool
// or a derived state label (with a "…" heartbeat while active) and running token
// totals. No elapsed clock, so it updates only as events arrive.
func teamLaneLine(ln *teamLane, nameW int) string {
	name := truncate(sanitizeTerminal(ln.name), maxTeamNameWidth)
	if pad := nameW - len([]rune(name)); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	if ln.lead {
		name += " [lead]"
	}
	return fmt.Sprintf("%s %s %s · %s · ↑%s ↓%s",
		teamGlyph(ln),
		teamMutCue(ln),
		name,
		teamLaneState(ln),
		humanizeTokens(ln.usage.InputTokens),
		humanizeTokens(ln.usage.OutputTokens))
}

// teamMutCue is the PERSISTENT per-member mutating cue (stable roster metadata): a
// "✎" for a mutating member (one running in an isolated fork with workspace-writing
// tools) and a space-matched "·" for a read-only member, so a mutating member stays
// visually distinct even while a tool name fills its state column. It is a fixed
// glyph-not-colour cue, never derived from the transient state label.
func teamMutCue(ln *teamLane) string {
	if ln.mutating {
		return "✎"
	}
	return "·"
}

// teamLaneState derives a member's current state label for the collapsed line: the
// running tool name when one is active, "done" once the member reported its
// terminal result, else "working". An ACTIVE member (not done) gets a trailing "…"
// heartbeat so a quiet card reads as in-flight rather than stalled (mirroring the
// "reasoning…" affordance); a done member has no ellipsis. The tool name is
// sanitized (server-derived). The mutating signal lives in teamMutCue, not here, so
// it persists once a tool name fills this label.
func teamLaneState(ln *teamLane) string {
	if ln.done {
		return "done"
	}
	label := "working"
	if ln.current != "" {
		label = sanitizeTerminal(ln.current)
	}
	return label + "…"
}

// renderTeamTrace renders a member lane's expanded trace: message lines (clamped,
// dim, prefixed "  ") interleaved with tool chips (✓/✗ name) carrying their bounded
// arg/result preview, in arrival order. A chip with a preview gets its own line
// ("  ✓ Grep — pattern: foo"); bare chips coalesce onto one wrapped row. Returns
// "" for an empty trace. All text is sanitized.
func (r *renderer) renderTeamTrace(ln *teamLane) string {
	if len(ln.trace) == 0 {
		return ""
	}
	muted := r.th.Style("muted")
	okStyle := r.th.Style("toolOk")
	errStyle := r.th.Style("toolErr")
	nameStyle := r.th.Style("toolName")

	var b strings.Builder
	var chips []string
	flush := func() {
		if len(chips) == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + wrapChips(chips, r.chipContentWidth()))
		chips = nil
	}
	writeLine := func(s string) {
		flush()
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	for i := range ln.trace {
		t := &ln.trace[i]
		switch t.kind {
		case teamTraceTool:
			glyph := okStyle.Render("✓")
			if t.isError {
				glyph = errStyle.Render("✗")
			}
			chip := glyph + " " + nameStyle.Render(sanitizeTerminal(t.name))
			if detail := sanitizeTerminal(oneLine(t.detail)); detail != "" {
				// A chip with a preview gets a dedicated line so its detail is readable.
				writeLine("  " + chip + muted.Render(" — "+truncate(detail, maxTeamDetailLen)))
			} else {
				chips = append(chips, chip)
			}
		case teamTraceMessage:
			writeLine("  " + muted.Render(truncate(sanitizeTerminal(oneLine(t.text)), maxTeamMessageLen)))
		}
	}
	flush()
	return b.String()
}

// teamResolvedLine is the muted one-line summary shown once the team run has
// ended: the round count, summed team token totals, and the stop reason (reusing
// the subagent stop-label mapping so labels stay consistent).
func teamResolvedLine(b *block) string {
	return fmt.Sprintf("team · %s · ↑%s ↓%s · stop:%s",
		plural(b.teamRounds, "round"),
		humanizeTokens(b.teamUsage.InputTokens),
		humanizeTokens(b.teamUsage.OutputTokens),
		subagentStopLabel(b.teamStop))
}

// oneLine collapses any internal newlines/tabs in a member message preview to
// single spaces so a multi-line forwarded fragment stays a single lane line.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t'
	}), " ")
}

// subagentStopLabel maps a child run's raw stop reason to the compact label shown
// on the resolved subagent line (done / max-tools / max-turns / error). An unknown
// or empty reason passes through verbatim so a new stop reason is never hidden.
func subagentStopLabel(stop string) string {
	switch stop {
	case "end_turn", "":
		return "done"
	case "max_tool_calls":
		return "max-tools"
	case "max_turns":
		return "max-turns"
	case "max_consecutive_failures":
		return "max-failures"
	case "cancelled":
		return "cancelled"
	case "error":
		return "error"
	default:
		return sanitizeTerminal(stop)
	}
}

// humanizeDuration renders a millisecond wall-clock duration compactly: sub-second
// as "Nms", under a minute as "N.Ns", else "Nm Ns". A non-positive duration (no
// clock) renders as "0ms".
func humanizeDuration(ms int64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	secs := float64(ms) / 1000.0
	if secs < 60 {
		return trimDecimal(secs) + "s"
	}
	m := int64(secs) / 60
	s := int64(secs) % 60
	return strconv.FormatInt(m, 10) + "m " + strconv.FormatInt(s, 10) + "s"
}

// resultBody renders a tool result body: full when expanded, else line-capped.
func resultBody(body string, expand bool) string {
	if expand {
		return sanitizeTerminal(strings.TrimRight(body, "\n"))
	}
	return truncateLines(body, maxToolResultLines)
}

// renderChangedFiles renders the session's changed-files summary as a muted,
// insertion-ordered list under a "✎ N files this session" header — the ctrl+t
// expansion of the header indicator (sharing its "✎" pencil glyph). Returns ""
// for an empty set. Paths are terminal-sanitized (they originate from
// server-relayed tool args).
func (r *renderer) renderChangedFiles(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	style := r.th.Style("muted")
	var b strings.Builder
	b.WriteString(style.Render("✎ " + plural(len(paths), "file") + " changed this session"))
	for _, p := range paths {
		b.WriteString("\n")
		b.WriteString(style.Render("  " + sanitizeTerminal(p)))
	}
	return b.String()
}

// mutatedPath returns the workspace path a file-MUTATING tool call touches, and
// ok=false for any read-only or unrecognised tool. It keys off the SAME arg
// shapes the diff renderer mirrors (editDiffArgs/writeDiffArgs, both carrying a
// "path" field — see the TestEditWriteArgKeysAreStable drift guard in
// internal/adapter/tools). The set of mutating tools is intentionally explicit
// (Edit, Write): adding a future mutating tool means adding a case here, not
// blanket-trusting every tool's "path" arg. Malformed args / empty path yield
// ("", false) so a garbled call never pollutes the changed-files set.
func mutatedPath(name, rawArgs string) (string, bool) {
	switch name {
	case "Edit":
		var args editDiffArgs
		if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil || args.Path == "" {
			return "", false
		}
		return args.Path, true
	case "Write":
		var args writeDiffArgs
		if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil || args.Path == "" {
			return "", false
		}
		return args.Path, true
	default:
		return "", false
	}
}

// renderToolDiff renders a colourised diff for the Edit and Write tools. It
// returns (rendered, true) when name is a diff-capable tool AND its args parse
// into the expected shape; otherwise (",", false) so the caller falls back to
// the existing pretty-JSON rendering. All server-derived text is sanitized
// before it reaches lipgloss.
func (r *renderer) renderToolDiff(name, rawArgs string, expand bool) (string, bool) {
	switch name {
	case "Edit":
		return r.renderEditDiff(rawArgs, expand)
	case "Write":
		return r.renderWriteDiff(rawArgs, expand)
	default:
		return "", false
	}
}

// editDiffArgs mirrors internal/adapter/tools/edit.go's editArgs JSON shape.
type editDiffArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// renderEditDiff renders an Edit as a red/green unified-style diff:
// removed (old_string) lines prefixed "-", added (new_string) lines prefixed
// "+", under a muted path header (with a "(replace all)" tag when set). Returns
// false on malformed/empty args so the caller falls back to JSON.
func (r *renderer) renderEditDiff(rawArgs string, expand bool) (string, bool) {
	var args editDiffArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil {
		return "", false
	}
	if args.Path == "" || (args.OldString == "" && args.NewString == "") {
		return "", false
	}

	// Size signal: removed/added line counts (empty side = 0 lines).
	removed := lineCount(args.OldString)
	added := lineCount(args.NewString)
	header := fmt.Sprintf("%s  -%d +%d", args.Path, removed, added)
	if args.ReplaceAll {
		header += " (replace all)"
	}
	var b strings.Builder
	b.WriteString(r.th.Style("diffMeta").Render(sanitizeTerminal(header)))
	b.WriteString("\n")
	b.WriteString(r.diffSide(args.OldString, "-", "diffRemove", expand))
	b.WriteString(r.diffSide(args.NewString, "+", "diffAdd", expand))
	return strings.TrimRight(b.String(), "\n"), true
}

// writeDiffArgs mirrors internal/adapter/tools/write.go's writeArgs JSON shape.
type writeDiffArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// renderWriteDiff renders a Write as an all-green "new content" block under a
// muted path header. Returns false on malformed args so the caller falls back.
//
// The header does NOT claim "new file": at ask time the harness doesn't know
// whether the path already exists, and a silent overwrite is MORE dangerous than
// a create — asserting "new file" would understate the risk at the approval gate.
// So it says "(overwrites if it exists)" instead, which holds in both cases.
func (r *renderer) renderWriteDiff(rawArgs string, expand bool) (string, bool) {
	var args writeDiffArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil {
		return "", false
	}
	if args.Path == "" {
		return "", false
	}
	header := fmt.Sprintf("%s · %s (overwrites if it exists)", args.Path, plural(lineCount(args.Content), "line"))
	var b strings.Builder
	b.WriteString(r.th.Style("diffMeta").Render(sanitizeTerminal(header)))
	if args.Content != "" {
		b.WriteString("\n")
		b.WriteString(r.diffSide(args.Content, "+", "diffAdd", expand))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// diffSide renders one side of a diff (all-removed or all-added): every line of
// text gets the prefix and the themed style, line-capped unless expanded. An
// empty side renders nothing. The text is sanitized (these go through lipgloss).
func (r *renderer) diffSide(text, prefix, slot string, expand bool) string {
	text = sanitizeTerminal(strings.TrimRight(text, "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	var marker string
	if !expand && len(lines) > maxDiffLines {
		extra := len(lines) - maxDiffLines
		lines = lines[:maxDiffLines]
		marker = collapseMarker(extra)
	}
	style := r.th.Style(slot)
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(style.Render(prefix + " " + ln))
		b.WriteString("\n")
	}
	if marker != "" {
		// The collapse marker is muted, not coloured as a diff line.
		b.WriteString(lipgloss.NewStyle().Render(marker))
		b.WriteString("\n")
	}
	return b.String()
}

// prettyJSON indents a raw JSON args string for display; non-JSON is returned
// as-is (single line). The result is terminal-sanitized since the payload is
// server-derived and rendered via lipgloss (not glamour). Empty/blank yields "".
func prettyJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return sanitizeTerminal(raw)
	}
	return sanitizeTerminal(buf.String())
}

// truncateLines clamps s to max lines, appending a "+N more lines · ctrl+t
// expand" affordance when it overflows. Used for tool results and diff sides,
// where ctrl+t is the way to see the rest.
func truncateLines(s string, maxLines int) string {
	return truncateLinesTail(s, maxLines, "")
}

// truncateLinesTail clamps s to maxLines lines, appending an overflow tail when
// it overflows. An empty tail uses the default "+N more lines · ctrl+t expand"
// collapse marker (the ctrl+t-referencing form for collapsible content); a
// non-empty tail is used verbatim instead — e.g. a neutral "…(truncated)" for
// already-expanded reasoning, which must NOT reference the toggle that revealed
// it. The (server-derived) body is terminal-sanitized.
func truncateLinesTail(s string, maxLines int, tail string) string {
	s = sanitizeTerminal(strings.TrimRight(s, "\n"))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	kept := lines[:maxLines]
	marker := tail
	if marker == "" {
		marker = collapseMarker(len(lines) - maxLines)
	}
	return strings.Join(kept, "\n") + "\n" + lipgloss.NewStyle().Render(marker)
}

// collapseMarker formats the "+N more line(s) · ctrl+t expand" affordance shown
// when a tool result or diff side is line-capped. The verb matches the footer
// help line's collapsed-state hint ("ctrl+t expand") — the expand/collapse pair
// is used consistently across help line, keybinding help, and this marker.
func collapseMarker(n int) string {
	noun := "lines"
	if n == 1 {
		noun = "line"
	}
	return "  … +" + strconv.Itoa(n) + " more " + noun + " · ctrl+t expand"
}

// lineCount returns the number of text lines in s (0 for empty, otherwise one
// more than the number of newlines, ignoring a single trailing newline). Used
// for the diff header size signals.
func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
