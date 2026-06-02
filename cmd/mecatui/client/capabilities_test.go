package client

import (
	"testing"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// TestCapabilitiesFrom covers the proto→plain translation, including the nil
// (older-server) case that MUST degrade to the all-false zero value rather than
// panic — the backward-compat guarantee.
func TestCapabilitiesFrom(t *testing.T) {
	tests := []struct {
		name string
		in   *mecatlv1.ServerCapabilities
		want Capabilities
	}{
		{
			name: "nil (older server) → all false",
			in:   nil,
			want: Capabilities{},
		},
		{
			name: "empty proto → all false",
			in:   &mecatlv1.ServerCapabilities{},
			want: Capabilities{},
		},
		{
			name: "all on",
			in: &mecatlv1.ServerCapabilities{
				Mcp:           true,
				SlashCommands: true,
				Memory:        true,
				Skills:        true,
				Teams:         true,
				Bash:          true,
			},
			want: Capabilities{
				MCP:           true,
				SlashCommands: true,
				Memory:        true,
				Skills:        true,
				Teams:         true,
				Bash:          true,
			},
		},
		{
			name: "mixed (each field maps independently)",
			in: &mecatlv1.ServerCapabilities{
				Mcp:    true,
				Memory: true,
				Bash:   true,
			},
			want: Capabilities{
				MCP:    true,
				Memory: true,
				Bash:   true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilitiesFrom(tc.in); got != tc.want {
				t.Fatalf("capabilitiesFrom(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}
