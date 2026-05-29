package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
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
// warning border + accent on the focused button make it unmissable.
func renderPermissionModal(th theme.Theme, ask pendingAsk, width, height int) string {
	title := th.Style("askTitle").Render("Permission required")

	// All ask.* fields are server-derived and rendered via lipgloss, so they MUST
	// be terminal-sanitized: an attacker who controls a tool result could
	// otherwise embed escapes to redraw/spoof this very approval modal. (Args is
	// sanitized inside prettyJSON.)
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(th.Style("toolName").Render(sanitizeTerminal(ask.Tool)) + "\n")
	if args := prettyJSON(ask.Args); args != "" {
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
