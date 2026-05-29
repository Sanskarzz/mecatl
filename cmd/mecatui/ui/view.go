package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View assembles the three-region layout (header / viewport / input / footer)
// into a tea.View. While a permission modal is open it overlays the modal,
// centred, over the conversation region. Bubble Tea v2 returns a tea.View struct
// (not a string); we set Content and request the alt screen.
func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = !m.deps.NoAltScreen
	v.WindowTitle = "mecatui"

	if m.phase == phaseFatal {
		v.Content = m.renderFatal()
		return v
	}

	header := m.renderHeader()
	footer := m.renderFooter()

	var body string
	if m.phase == phaseAwaitingApproval {
		body = renderPermissionModal(m.deps.Theme, m.ask, m.width, m.vp.Height())
	} else {
		body = m.vp.View()
	}

	input := m.renderInput()

	v.Content = strings.Join([]string{header, body, input, footer}, "\n")
	return v
}

// renderHeader is the top bar: session id · model · mode · server.
func (m Model) renderHeader() string {
	sid := m.sessionID
	if sid == "" {
		sid = "connecting…"
	}
	parts := []string{
		"mecatui",
		"session " + short(sid),
	}
	if m.deps.Model != "" {
		parts = append(parts, m.deps.Model)
	}
	if m.deps.Mode != "" {
		parts = append(parts, "mode "+m.deps.Mode)
	}
	if m.deps.Server != "" {
		parts = append(parts, m.deps.Server)
	}
	line := strings.Join(parts, "  ·  ")
	return m.deps.Theme.Style("header").Width(m.widthOr(80)).Render(line)
}

// renderFooter is the status bar: spinner + active tool + status + usage.
func (m Model) renderFooter() string {
	var left string
	switch m.phase {
	case phaseRunning:
		spin := m.sp.View()
		if m.activeTool != "" {
			left = fmt.Sprintf("%s Running %s…", spin, m.activeTool)
		} else {
			left = spin + " thinking…"
		}
	case phaseAwaitingApproval:
		left = m.deps.Theme.Style("askTitle").Render("⚠ awaiting approval")
	case phaseConnecting:
		left = m.sp.View() + " connecting…"
	default:
		left = m.statusMsg
		if left == "" {
			left = "ready"
		}
	}

	usage := fmt.Sprintf("in %d · out %d · cache %d",
		m.usage.InputTokens, m.usage.OutputTokens, m.usage.CacheReadTokens)

	help := "enter send · shift+enter newline · esc cancel · ctrl+c quit"

	width := m.widthOr(80)
	line := left
	// Right-align usage when there's room.
	gap := width - lipgloss.Width(left) - lipgloss.Width(usage) - 2
	if gap > 0 {
		line = left + strings.Repeat(" ", gap) + usage
	}
	footer := m.deps.Theme.Style("footer").Width(width).Render(line)
	return footer + "\n" + m.deps.Theme.Style("muted").Render(help)
}

// renderInput renders the textarea, dimmed while a run is active.
func (m Model) renderInput() string {
	return m.ta.View()
}

// renderFatal renders a centred fatal-error panel.
func (m Model) renderFatal() string {
	msg := m.deps.Theme.Style("errorText").Render("connection failed") + "\n\n" +
		m.deps.Theme.Style("muted").Render(m.fatalErr) + "\n\n" +
		m.deps.Theme.Style("muted").Render("press ctrl+c to quit")
	card := m.deps.Theme.Style("askCard").Render(msg)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
	}
	return card
}

// short truncates a long id for the header.
func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// widthOr returns the terminal width or a fallback when unset (pre-first-resize).
func (m Model) widthOr(fallback int) int {
	if m.width > 0 {
		return m.width
	}
	return fallback
}
