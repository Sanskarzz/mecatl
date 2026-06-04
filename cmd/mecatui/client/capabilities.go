package client

import (
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// Capabilities is the proto-free mirror of mecatlv1.ServerCapabilities: which
// optional features the connected server has enabled. The ui renders honest
// affordances from it (advertise only reachable features; explain empty
// inventories as "not enabled" vs "enabled but empty") WITHOUT importing proto.
// All-false is the safe default (an older server omits the field).
type Capabilities struct {
	MCP           bool
	SlashCommands bool
	Memory        bool
	Skills        bool
	Teams         bool
	Agents        bool
	Bash          bool
	// Image/Audio report whether the wired provider consumes that media kind. They
	// gate the @-mention file-attach UX: a client refuses to send a part the
	// server's provider cannot read (an old server with no field → false → degrade).
	Image bool
	Audio bool
}

// capabilitiesFrom maps a proto ServerCapabilities (nil-safe) to the plain
// struct. A nil message (older server) yields the all-false zero value.
func capabilitiesFrom(c *mecatlv1.ServerCapabilities) Capabilities {
	if c == nil {
		return Capabilities{}
	}
	return Capabilities{
		MCP:           c.GetMcp(),
		SlashCommands: c.GetSlashCommands(),
		Memory:        c.GetMemory(),
		Skills:        c.GetSkills(),
		Teams:         c.GetTeams(),
		Agents:        c.GetAgents(),
		Bash:          c.GetBash(),
		Image:         c.GetImage(),
		Audio:         c.GetAudio(),
	}
}
