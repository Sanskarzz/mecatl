package ui

import (
	"strings"
	"testing"
)

// TestHelpOverlayReflectsPlatformMarking guards against re-hardcoding the
// pgup/pgdn literal: with the platform pinned to mac, the help overlay must
// show the Mac scroll-key marking (fn+↑/fn+↓), not the bare PC literal. This
// proves the help renderer consults platform.ScrollKeysMarking() rather than a
// hardcoded string — no hardcoded "pgup/pgdn" key column can produce the
// fn+↑/fn+↓ prefix, so the positive assertion below can only hold if the
// helper was consulted.
func TestHelpOverlayReflectsPlatformMarking(t *testing.T) {
	t.Setenv("MECATUI_TEST_PLATFORM", "mac")

	out := m_helpBody(allOnCaps())

	// The Mac branch's marking starts with fn+↑ — only reachable via the helper.
	if !strings.Contains(out, "fn+↑/fn+↓") {
		t.Errorf("help overlay did not reflect the Mac scroll-key marking under the mac platform; want substring %q:\n%s", "fn+↑/fn+↓", out)
	}
	// The canonical pgup/pgdn name is always present (inside the Mac marking's
	// parens), so assert it survives too.
	if !strings.Contains(out, "pgup/pgdn") {
		t.Errorf("help overlay lost the canonical pgup/pgdn name under the mac platform:\n%s", out)
	}
}
