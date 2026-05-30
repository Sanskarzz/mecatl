package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// pendingAsk holds the state of an open permission modal. AskID is the exact
// correlation key sent back in ResumeApproval — it is never derived from the
// tool name. allowFocused tracks which button is highlighted (Allow vs Deny).
type pendingAsk struct {
	AskID        string
	Tool         string
	Args         string
	Reason       string
	allowFocused bool
}

// renderPermissionModal renders the centred approval card. It is drawn with
// lipgloss.Place over the available area so it reads as a modal overlay. The
// warning border + accent on the focused button make it unmissable. It is a
// method on renderer so it can reuse renderToolDiff: for an Edit/Write ask the
// concrete colourised diff of the change is shown in place of the raw JSON args,
// so the operator approves a real edit rather than an opaque blob. expand is the
// global details toggle (ctrl+t): when on, the diff renders in full instead of
// line-capped, so the collapse marker's "ctrl+t expand" hint is truthful — the
// operator can genuinely reveal every line being authorized before deciding.
func (r *renderer) renderPermissionModal(ask pendingAsk, expand bool, width, height int) string {
	th := r.th
	title := th.Style("askTitle").Render("Permission required")

	// All ask.* fields are server-derived and rendered via lipgloss, so they MUST
	// be terminal-sanitized: an attacker who controls a tool result could
	// otherwise embed escapes to redraw/spoof this very approval modal. (Args is
	// sanitized inside prettyJSON; the diff path sanitizes internally.)
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(th.Style("toolName").Render(sanitizeTerminal(ask.Tool)) + "\n")
	// Prefer a concrete diff for Edit/Write. Collapsed by default (line-capped, so
	// a huge Write can't grow the modal off-screen); ctrl+t (expand) reveals the
	// full diff right here at the gate. Fall back to pretty JSON for any other
	// tool, or when the Edit/Write args don't parse into the expected shape.
	if diff, ok := r.renderToolDiff(ask.Tool, ask.Args, expand); ok {
		if diff != "" {
			b.WriteString(th.Style("muted").Render("changes:") + "\n")
			b.WriteString(diff + "\n")
		}
	} else if args := prettyJSON(ask.Args); args != "" {
		b.WriteString(th.Style("toolArgs").Render(args) + "\n")
	}
	if ask.Reason != "" {
		b.WriteString("\n" + th.Style("muted").Render(sanitizeTerminal(ask.Reason)) + "\n")
	}

	allow := th.Style("askButton").Render("[A]llow")
	deny := th.Style("askButton").Render("[D]eny")
	if ask.allowFocused {
		allow = th.Style("askButtonActive").Render("[A]llow")
	} else {
		deny = th.Style("askButtonActive").Render("[D]eny")
	}
	buttons := lipgloss.JoinHorizontal(lipgloss.Top, allow, "  ", deny)
	b.WriteString("\n" + buttons)

	card := th.Style("askCard").Render(b.String())

	if width <= 0 || height <= 0 {
		return card
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, card)
}
