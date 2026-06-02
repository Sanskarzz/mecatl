package ui

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// emptyMCPState is an MCP overlay state with a finished (non-loading) empty
// fetch — the "nothing to show" condition the caps-aware copy disambiguates.
func emptyMCPState(v mcpView) mcpState { return mcpState{view: v} }

// TestMCPEmptyStateCapsAware is the Option-C payoff: the SAME empty inventory
// reads "not enabled" when caps.MCP is false and "configured but empty" when
// caps.MCP is true, across the panel/resources/prompts views.
func TestMCPEmptyStateCapsAware(t *testing.T) {
	th := aztec()
	off := client.Capabilities{}         // MCP off
	on := client.Capabilities{MCP: true} // MCP on, but inventory empty

	cases := []struct {
		name    string
		view    mcpView
		offWant string
		onWant  string
	}{
		{"panel", mcpPanel, "MCP is not enabled on this server", "No MCP sources configured on this server"},
		{"resources", mcpResources, "MCP is not enabled on this server", "No resources advertised"},
		{"prompts", mcpPrompts, "MCP is not enabled on this server", "No prompts advertised"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offOut := stripANSIstr(renderMCPOverlay(th, emptyMCPState(tc.view), off, 100, 24))
			if !strings.Contains(offOut, tc.offWant) {
				t.Errorf("MCP off: want %q in:\n%s", tc.offWant, offOut)
			}
			// The off copy carries the remedy.
			if !strings.Contains(offOut, "Run a full mecated") {
				t.Errorf("MCP off copy should carry the remedy:\n%s", offOut)
			}
			onOut := stripANSIstr(renderMCPOverlay(th, emptyMCPState(tc.view), on, 100, 24))
			if !strings.Contains(onOut, tc.onWant) {
				t.Errorf("MCP on-but-empty: want %q in:\n%s", tc.onWant, onOut)
			}
			if strings.Contains(onOut, "not enabled") {
				t.Errorf("MCP on-but-empty must NOT say 'not enabled':\n%s", onOut)
			}
		})
	}
}

// TestPaletteEmptyNoteCapsAware asserts the "/" palette note distinguishes
// "not enabled" from "none found", and is empty for a non-command input.
func TestPaletteEmptyNoteCapsAware(t *testing.T) {
	th := aztec()
	var st paletteState // closed, no rows

	off := stripANSIstr(renderPalette(th, st, client.Capabilities{}, "/", 100))
	if !strings.Contains(off, "slash commands are not enabled on this server") {
		t.Errorf("palette (commands off) should say 'not enabled':\n%s", off)
	}
	on := stripANSIstr(renderPalette(th, st, client.Capabilities{SlashCommands: true}, "/foo", 100))
	if !strings.Contains(on, "no slash commands found in this workspace") {
		t.Errorf("palette (commands on, empty) should say 'none found':\n%s", on)
	}
	// Non-command input: no note.
	if renderPalette(th, st, client.Capabilities{}, "hello", 100) != "" {
		t.Errorf("palette should render nothing for a non-command input")
	}
	// Dismissed: no note even on a command line.
	dismissed := paletteState{dismissed: true}
	if renderPalette(th, dismissed, client.Capabilities{}, "/", 100) != "" {
		t.Errorf("palette should render nothing while dismissed")
	}
}
