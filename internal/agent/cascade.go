package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
)

// Cascade tier defaults. Each is a knob on CascadeCompactor; the zero value of
// the struct uses these.
const (
	// cascadeKeepLastTurns is how many trailing messages the cascade always
	// preserves verbatim (the recent working set), never touched by any tier.
	cascadeKeepLastTurns = 6
	// cascadeStripToolBodyChars is the per-tool-result budget tier 2 keeps; longer
	// bodies are truncated with an elision marker. Smaller than the heuristic's
	// budget because the cascade strips more aggressively, in stages.
	cascadeStripToolBodyChars = 200
	// cascadeMaxCollapseChars is the body size above which tier 3 collapses a file
	// read entirely to a path+size pointer.
	cascadeMaxCollapseChars = 1024
)

// CascadeCompactor is a tiered, cheapest-first Compactor (harness pattern 5,
// doc 07 §4 / doc 08 #12). It applies up to four tiers in order, stopping as
// soon as the history fits the budget:
//
//  1. snip — drop the oldest low-value turns (assistant/tool pairs and old user
//     chatter) outside the preserved head (system + goal) and tail (recent K).
//  2. strip — truncate large tool-result bodies to a small budget, eliding the
//     rest with a marker (the lightest-touch "tool-result clearing").
//  3. collapse — replace large file-read tool results with a path+size pointer,
//     dropping the body entirely (the model can re-read on demand).
//  4. summarize — if still over budget AND an LLMProvider is injected, ask the
//     model for a compact summary of the oldest segment and replace it. With no
//     LLM injected the cascade STOPS at tier 3, remaining fully deterministic and
//     offline-testable.
//
// Across every tier it preserves the system prompt, the user goal, all touched
// file paths (as a synthesised summary message), and the recent tail. It drops
// file bodies, verbose tool output, and old stack traces — the doc-08 "what to
// preserve / what to drop" contract.
type CascadeCompactor struct {
	// Counter sizes the history between tiers; nil → HeuristicTokenCounter.
	Counter TokenCounter
	// BudgetTokens is the target the cascade reduces toward. Tiers stop running
	// once the history is at or below it. Zero means "run every deterministic
	// tier once" (snip→strip→collapse), which is the offline default.
	BudgetTokens int
	// KeepLastTurns overrides cascadeKeepLastTurns when > 0.
	KeepLastTurns int
	// StripToolBodyChars overrides cascadeStripToolBodyChars when > 0.
	StripToolBodyChars int
	// MaxCollapseChars overrides cascadeMaxCollapseChars when > 0.
	MaxCollapseChars int

	// LLM, when non-nil, enables tier 4 (LLM summary of the oldest segment). When
	// nil the cascade stops at tier 3 and never makes a model call.
	LLM port.LLMProvider
	// Model is the model identifier passed to the LLM on the tier-4 summary call.
	Model string
}

// Compile-time assertion that the cascade satisfies the Compactor seam.
var _ Compactor = CascadeCompactor{}

// Compact implements Compactor by running the tiered cascade. It always returns
// a reduced (or equal) history and a human-readable summary of what each tier
// did; it returns an error only when a tier-4 LLM call fails (tiers 1–3 cannot
// fail).
func (c CascadeCompactor) Compact(ctx context.Context, conv *session.Conversation) ([]session.Message, string, error) {
	counter := c.counter()
	keep := cascadeKeepLastTurns
	if c.KeepLastTurns > 0 {
		keep = c.KeepLastTurns
	}

	// Always synthesise a paths-summary message from the FULL history first, so a
	// file path survives even if every turn that touched it is dropped later.
	paths := touchedPaths(conv.Messages)

	msgs := conv.Messages
	var notes []string

	// Partition into the preserved head (leading system messages + first user
	// goal), the compactible middle, and the preserved tail (last keep messages).
	head, headIdx := preservedHead(msgs)
	cut := len(msgs) - keep
	if cut < len(headIdx) {
		cut = len(headIdx)
	}
	if cut > len(msgs) {
		cut = len(msgs)
	}
	tail := msgs[cut:]
	middle := middleMessages(msgs, headIdx, cut)

	// Tier 1: snip — drop the oldest low-value turns from the middle. We keep at
	// most half the middle (the more recent half), dropping the older half, which
	// tend to be settled work the agent has already acted on.
	before := len(middle)
	middle = snip(middle)
	if len(middle) < before {
		notes = append(notes, fmt.Sprintf("snip: dropped %d old turns", before-len(middle)))
	}

	if c.fits(counter, head, middle, tail, paths) {
		return c.assemble(head, middle, tail, paths), summaryNote(notes), nil
	}

	// Tier 2: strip — truncate large tool-result bodies in the middle.
	stripBudget := cascadeStripToolBodyChars
	if c.StripToolBodyChars > 0 {
		stripBudget = c.StripToolBodyChars
	}
	stripped := 0
	for i, m := range middle {
		nm := truncateToolBody(m, stripBudget)
		if nm.ToolResult != middle[i].ToolResult {
			stripped++
		}
		middle[i] = nm
	}
	if stripped > 0 {
		notes = append(notes, fmt.Sprintf("strip: truncated %d large tool outputs", stripped))
	}

	if c.fits(counter, head, middle, tail, paths) {
		return c.assemble(head, middle, tail, paths), summaryNote(notes), nil
	}

	// Tier 3: collapse — replace large tool-read bodies with a path+size pointer,
	// dropping the body entirely.
	collapseBudget := cascadeMaxCollapseChars
	if c.MaxCollapseChars > 0 {
		collapseBudget = c.MaxCollapseChars
	}
	collapsed := 0
	for i, m := range middle {
		nm, did := collapseToolBody(m, collapseBudget)
		if did {
			collapsed++
		}
		middle[i] = nm
	}
	if collapsed > 0 {
		notes = append(notes, fmt.Sprintf("collapse: replaced %d file bodies with pointers", collapsed))
	}

	if c.LLM == nil || c.fits(counter, head, middle, tail, paths) {
		return c.assemble(head, middle, tail, paths), summaryNote(notes), nil
	}

	// Tier 4: summarize — ask the LLM to summarise the (already stripped/collapsed)
	// middle segment, replacing it with a single user message. Only reached when an
	// LLM is injected and tiers 1–3 left the history over budget.
	summary, err := c.summarize(ctx, head, middle)
	if err != nil {
		return nil, "", fmt.Errorf("agent: cascade tier-4 summarize: %w", err)
	}
	middle = []session.Message{session.NewUserMessage(summary)}
	notes = append(notes, "summarize: replaced oldest segment with an LLM summary")
	return c.assemble(head, middle, tail, paths), summaryNote(notes), nil
}

// counter returns the configured TokenCounter or the heuristic default.
func (c CascadeCompactor) counter() TokenCounter {
	if c.Counter != nil {
		return c.Counter
	}
	return HeuristicTokenCounter{}
}

// fits reports whether the assembled history is at or below BudgetTokens. A zero
// budget means "no target": fits reports false, so every deterministic tier runs
// once (the offline default), and true only after the last deterministic tier so
// the cascade always terminates without an LLM.
func (c CascadeCompactor) fits(counter TokenCounter, head, middle, tail []session.Message, paths []string) bool {
	if c.BudgetTokens <= 0 {
		return false
	}
	combined := c.assemble(head, middle, tail, paths)
	return counter.CountMessages(combined) <= c.BudgetTokens
}

// assemble stitches the preserved head, the synthesised paths summary, the
// compacted middle, and the preserved tail into the final history.
func (CascadeCompactor) assemble(head, middle, tail []session.Message, paths []string) []session.Message {
	out := make([]session.Message, 0, len(head)+len(middle)+len(tail)+1)
	out = append(out, head...)
	out = append(out, session.NewUserMessage(buildSummary(paths)))
	out = append(out, middle...)
	out = append(out, tail...)
	return out
}

// summarize asks the injected LLM to compress the middle segment into a compact
// "what we did / decided / what's pending" block, preserving the doc-08 signal
// list. It consumes the whole stream and returns the assembled text.
func (c CascadeCompactor) summarize(ctx context.Context, head, middle []session.Message) (string, error) {
	instruction := session.NewUserMessage(
		"Summarise the conversation so far into a compact block. PRESERVE: the current plan, " +
			"decisions made and why, unresolved questions and known errors, and every file path touched. " +
			"DROP: raw file contents, verbose tool output, and old stack traces. Respond with the summary only.",
	)
	reqMsgs := make([]session.Message, 0, len(head)+len(middle)+1)
	reqMsgs = append(reqMsgs, head...)
	reqMsgs = append(reqMsgs, middle...)
	reqMsgs = append(reqMsgs, instruction)

	seq, err := c.LLM.Stream(ctx, port.LLMRequest{
		System:   prompt.Layered{StablePrefix: "You are a context-compaction summariser."},
		Messages: reqMsgs,
		Model:    c.Model,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for chunk, cerr := range seq {
		if cerr != nil {
			return "", cerr
		}
		if chunk.Kind == port.ChunkText {
			b.WriteString(chunk.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "[compaction summary unavailable]"
	}
	return "[earlier turns summarised]\n" + out, nil
}

// preservedHead returns the leading system messages plus the first user goal,
// together with the set of source indices they occupy (so middleMessages can
// exclude them). The head is preserved verbatim across every tier.
func preservedHead(msgs []session.Message) (head []session.Message, idx []int) {
	for i, m := range msgs {
		if m.Role == session.RoleSystem {
			head = append(head, m)
			idx = append(idx, i)
		}
	}
	for i, m := range msgs {
		if m.Role == session.RoleUser {
			head = append(head, m)
			idx = append(idx, i)
			break
		}
	}
	return head, idx
}

// middleMessages returns the messages strictly between the head and the tail cut:
// every message at index < cut that is not part of the preserved head.
func middleMessages(msgs []session.Message, headIdx []int, cut int) []session.Message {
	inHead := make(map[int]struct{}, len(headIdx))
	for _, i := range headIdx {
		inHead[i] = struct{}{}
	}
	out := make([]session.Message, 0, cut)
	for i := 0; i < cut && i < len(msgs); i++ {
		if _, ok := inHead[i]; ok {
			continue
		}
		out = append(out, msgs[i])
	}
	return out
}

// snip drops the older half of the middle segment, keeping the more recent half
// (the work nearer the current task). It is the cheapest tier: no body rewriting,
// just dropping settled turns. An empty or single-element middle is unchanged.
func snip(middle []session.Message) []session.Message {
	if len(middle) <= 1 {
		return middle
	}
	drop := len(middle) / 2
	return middle[drop:]
}

// collapseToolBody replaces a large tool-result body with a compact pointer that
// records the call id and the original body size, dropping the content. It
// reports whether it collapsed the message. Non-tool messages and small bodies
// pass through unchanged.
func collapseToolBody(m session.Message, budget int) (session.Message, bool) {
	if m.Role != session.RoleTool || m.ToolResult == nil {
		return m, false
	}
	if len(m.ToolResult.Content) <= budget {
		return m, false
	}
	pointer := fmt.Sprintf("<<tool_result_collapsed id=%s size=%dB; re-run the tool to retrieve it>>",
		m.ToolResult.CallID, len(m.ToolResult.Content))
	if m.ToolResult.IsError {
		return session.NewToolMessage(session.NewToolError(m.ToolResult.CallID, pointer)), true
	}
	return session.NewToolMessage(session.NewToolResult(m.ToolResult.CallID, pointer)), true
}

// summaryNote renders the per-tier notes into a single human-readable compaction
// summary string for the EvCompaction event.
func summaryNote(notes []string) string {
	var b strings.Builder
	b.WriteString("[conversation compacted via tiered cascade] ")
	b.WriteString("System prompt, goal, decisions, and touched file paths preserved; ")
	b.WriteString("file bodies and verbose tool output dropped.")
	if len(notes) > 0 {
		b.WriteString("\nTiers applied: ")
		b.WriteString(strings.Join(notes, "; "))
	}
	return b.String()
}
