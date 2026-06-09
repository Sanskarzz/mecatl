package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// pendingAsk holds the state of an open permission modal. AskID is the exact
// correlation key sent back in ResumeApproval — it is never derived from the
// tool name. focus tracks which button is highlighted (0=allow-once, 1=always,
// 2=deny). offerAlways gates the middle "always" button: it is offered only for
// the MAIN agent's asks, never for a surfaced subagent ask (a child engine's
// permission policy has a nil learn store, so always-allow would be a silent
// no-op there).
type pendingAsk struct {
	AskID       string
	Tool        string
	Args        string
	Reason      string
	focus       int
	offerAlways bool
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

	// Three buttons when always-allow is offered (main-agent asks), two otherwise
	// (surfaced subagent asks). focus indexes {allow-once, always, deny}; for a
	// two-button modal focus only ever takes 0 (allow) or 2 (deny).
	btnStyle := func(idx int) string {
		if ask.focus == idx {
			return "askButtonActive"
		}
		return "askButton"
	}
	allow := th.Style(btnStyle(0)).Render("[A]llow")
	deny := th.Style(btnStyle(2)).Render("[D]eny")
	var buttons string
	if ask.offerAlways {
		always := th.Style(btnStyle(1)).Render("Al[w]ays")
		buttons = lipgloss.JoinHorizontal(lipgloss.Top, allow, "  ", always, "  ", deny)
	} else {
		buttons = lipgloss.JoinHorizontal(lipgloss.Top, allow, "  ", deny)
	}
	b.WriteString("\n" + buttons)
	if ask.offerAlways {
		b.WriteString("\n" + th.Style("muted").Render("al[w]ays allows this exact command for the rest of this session"))
	}

	return centerCard(th, b.String(), width, height)
}
