package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// teaminspect.go implements the on-demand, PULL member-transcript inspection tool
// (requirement E). It is a parent-catalog tool the parent LLM calls DELIBERATELY to
// read ONE persisted team member's transcript by (team id, member name). It does NOT
// auto-inject any transcript: the pulled transcript enters the parent Conversation
// only as this tool's own ToolResult — exactly like every tool, and the parent's
// explicit choice — so gauntlet #7's "no AUTO-injection of member transcripts into
// the parent context" property is preserved.

// inspectMemberToolName is the catalog name of the member-inspection tool.
const inspectMemberToolName = "InspectMember"

// maxInspectMessages caps how many trailing member messages the transcript renders,
// and maxInspectMessageRunes caps each rendered message body — together bounding the
// ToolResult so a long member transcript can never be copied verbatim onto the
// parent's conversation (mirroring the clampPreview discipline the team stream uses).
const (
	maxInspectMessages     = 40
	maxInspectMessageRunes = 1000
	maxInspectTotalRunes   = 8000
)

// InspectMemberTool reads one persisted team member session via a port.SessionStore
// and returns a BOUNDED rendering of its conversation as its ToolResult. It is PULL
// and read-only.
type InspectMemberTool struct {
	// store reads member sessions by id. Injected by the composition root; the tool
	// consumes the port.SessionStore interface, never a concrete adapter.
	store port.SessionStore
}

// inspectMemberArgs is the model-supplied argument payload.
type inspectMemberArgs struct {
	// TeamID is the team id the parent saw on the Team ToolResult / EvTeamStart.
	TeamID string `json:"team_id"`
	// Member is the member name whose transcript to read.
	Member string `json:"member"`
}

// inspectMemberSchema is the JSON schema the model sees for the tool's arguments.
var inspectMemberSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "team_id": {"type": "string", "description": "The team id (as seen on the Team tool result / team start event)."},
    "member": {"type": "string", "description": "The member's name."}
  },
  "required": ["team_id", "member"]
}`)

// NewInspectMemberTool constructs the InspectMember tool over a session store. store
// must be non-nil; NewInspectMemberTool panics otherwise (a composition-root
// programming error — the tool has nothing to read without a store).
func NewInspectMemberTool(store port.SessionStore) tool.Tool {
	if store == nil {
		panic("agent: NewInspectMemberTool requires a non-nil session store")
	}
	return &InspectMemberTool{store: store}
}

// Spec returns the model-facing specification for the InspectMember tool.
func (*InspectMemberTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: inspectMemberToolName,
		Description: "Read one team member's full transcript by team id and member name. Each " +
			"call folds that member's transcript into this conversation and consumes context " +
			"budget, so prefer the team's summary and use this ONLY when the summary is " +
			"insufficient and you need a specific member's detailed work. Returns a bounded " +
			"rendering of that member's conversation.",
		Schema: inspectMemberSchema,
	}
}

// ReadOnly reports that InspectMember only READS the store (no workspace mutation),
// so the dispatcher may run it read-parallel.
func (*InspectMemberTool) ReadOnly() bool { return true }

// Execute loads the member's persisted session (id derived via the SHARED
// MemberSessionID helper, so it cannot drift from the supervisor's save id) and
// renders a bounded transcript. An unknown id is a model-addressable error (the team
// may not have run, or the member never started).
func (t *InspectMemberTool) Execute(ctx context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args inspectMemberArgs
	if msg, ok := session.ParseArgs(call, &args); !ok {
		return session.NewToolError(call.ID, "InspectMember: "+msg), nil
	}
	teamID := strings.TrimSpace(args.TeamID)
	member := strings.TrimSpace(args.Member)
	if teamID == "" || member == "" {
		return session.NewToolError(call.ID, "InspectMember: both 'team_id' and 'member' are required"), nil
	}

	id := MemberSessionID(teamID, member)
	sess, err := t.store.Load(ctx, id)
	switch {
	case errors.Is(err, port.ErrSessionNotFound) || (err == nil && sess == nil):
		// Genuine not-found: a model-addressable miss the parent can reason about.
		return session.NewToolError(call.ID, fmt.Sprintf(
			"InspectMember: no transcript for member %q in team %q; the team may not have run or the member never started",
			member, teamID)), nil
	case err != nil:
		// A real infrastructure failure (I/O, decode) — surfaced DISTINCTLY so a broken
		// store is not silently misreported as "no transcript". Still a model-addressable
		// error result (never a harness error) so the loop continues.
		return session.NewToolError(call.ID, fmt.Sprintf(
			"InspectMember: failed to load member %q in team %q: %v", member, teamID, err)), nil
	}
	return session.NewToolResult(call.ID, renderMemberTranscript(member, teamID, sess)), nil
}

// renderTranscript renders the BOUNDED trailing tail of a member session's
// conversation into a single, readable string for the parent's ToolResult. It clamps
// the number of messages (maxInspectMessages), each message body
// (maxInspectMessageRunes), and the overall length (maxInspectTotalRunes), so an
// arbitrarily long member transcript can never be copied verbatim.
func renderMemberTranscript(member, teamID string, sess *session.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Transcript of member %q in team %q:\n", member, teamID)

	msgs := sess.Conversation.Messages
	if len(msgs) == 0 {
		b.WriteString("(no messages)\n")
		return b.String()
	}
	// Keep only the trailing tail when the transcript is long.
	if len(msgs) > maxInspectMessages {
		fmt.Fprintf(&b, "(showing the last %d of %d messages)\n", maxInspectMessages, len(msgs))
		msgs = msgs[len(msgs)-maxInspectMessages:]
	}

	for _, m := range msgs {
		line := renderInspectMessage(m)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
		if b.Len() >= maxInspectTotalRunes {
			b.WriteString("… [transcript truncated]\n")
			break
		}
	}
	return b.String()
}

// renderInspectMessage renders one conversation message as a bounded line.
func renderInspectMessage(m session.Message) string {
	switch m.Role {
	case session.RoleAssistant:
		var parts []string
		if txt := strings.TrimSpace(m.Text); txt != "" {
			parts = append(parts, clampRunes(txt, maxInspectMessageRunes))
		}
		for _, c := range m.ToolCalls {
			parts = append(parts, fmt.Sprintf("[called %s]", c.Name))
		}
		if len(parts) == 0 {
			return ""
		}
		return "assistant: " + strings.Join(parts, " ")
	case session.RoleTool:
		if m.ToolResult == nil {
			return ""
		}
		return "tool result: " + clampRunes(strings.TrimSpace(m.ToolResult.Content), maxInspectMessageRunes)
	case session.RoleUser:
		return "user: " + clampRunes(strings.TrimSpace(m.Text), maxInspectMessageRunes)
	case session.RoleSystem:
		// The system prompt is harness-authored boilerplate; skip it to keep the
		// transcript focused on the member's actual work.
		return ""
	default:
		return ""
	}
}

// clampRunes clamps s to at most n runes, appending an ellipsis on overflow. It is
// rune-aware so it never splits a multi-byte character.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Compile-time assertion that InspectMemberTool satisfies the Tool contract.
var _ tool.Tool = (*InspectMemberTool)(nil)
