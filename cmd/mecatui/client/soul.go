package client

import (
	"context"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The soul (persona) inspection surface: a plain client-owned struct mirroring the
// proto SoulInfo message, the unary RPC wrapper that maps proto → the struct, and
// the tea.Cmd constructor the ui's /soul panel calls. As with the skills/agents
// surfaces, NO proto type leaks past this file — the ui renders purely from the
// struct + msg below, and the mapping is exercised offline against a fake client.

// SoulProvenance is the proto-free mirror of mecatlv1.SoulProvenance: where the
// selected (or dropped) soul originated. The ui renders a trust label from it
// WITHOUT importing proto.
type SoulProvenance int

const (
	// SoulProvenanceNone means no soul was selected.
	SoulProvenanceNone SoulProvenance = iota
	// SoulProvenanceUser means the user-scoped soul (always trusted).
	SoulProvenanceUser
	// SoulProvenanceProject means the project-scoped soul (trusted only with
	// --trust-project).
	SoulProvenanceProject
)

// Soul is the resolved soul snapshot (proto SoulInfo, proto-free): the persona
// content plus its provenance/trust/drift metadata. The content is the bytes that
// reach the prompt; the ui shows it read-only (it never edits the soul).
type Soul struct {
	Content    string
	SizeBytes  int64
	SHA256     string
	Present    bool
	Provenance SoulProvenance
	Trusted    bool
	Drifted    bool
}

// SoulMsg carries a GetSoul result for the /soul panel. Err is set on failure;
// the panel surfaces it rather than silently degrading.
type SoulMsg struct {
	Soul Soul
	Err  error
}

// GetSoul fetches the resolved soul snapshot (a startup snapshot server-side).
func (c *Client) GetSoul(ctx context.Context) (Soul, error) {
	resp, err := c.svc.GetSoul(ctx, &mecatlv1.GetSoulRequest{})
	if err != nil {
		return Soul{}, err
	}
	return mapSoul(resp.GetSoul()), nil
}

// mapSoul maps a proto SoulInfo (nil-safe) to the plain struct.
func mapSoul(in *mecatlv1.SoulInfo) Soul {
	if in == nil {
		return Soul{}
	}
	return Soul{
		Content:    in.GetContent(),
		SizeBytes:  in.GetSizeBytes(),
		SHA256:     in.GetSha256(),
		Present:    in.GetPresent(),
		Provenance: mapSoulProvenance(in.GetProvenance()),
		Trusted:    in.GetTrusted(),
		Drifted:    in.GetDrifted(),
	}
}

// mapSoulProvenance maps the proto enum to the proto-free counterpart.
func mapSoulProvenance(p mecatlv1.SoulProvenance) SoulProvenance {
	switch p {
	case mecatlv1.SoulProvenance_SOUL_PROVENANCE_USER:
		return SoulProvenanceUser
	case mecatlv1.SoulProvenance_SOUL_PROVENANCE_PROJECT:
		return SoulProvenanceProject
	default:
		return SoulProvenanceNone
	}
}

// SoulFetcher is the subset of *Client the ui's /soul panel needs. Splitting it
// out keeps the ui injectable with a fake for offline tests; *Client satisfies it.
type SoulFetcher interface {
	GetSoul(ctx context.Context) (Soul, error)
}

// GetSoulCmd fetches the soul snapshot off the update goroutine; the result
// (success or error) arrives as a SoulMsg.
func GetSoulCmd(ctx context.Context, c SoulFetcher) tea.Cmd {
	return func() tea.Msg {
		soul, err := c.GetSoul(ctx)
		if err != nil {
			return SoulMsg{Err: err}
		}
		return SoulMsg{Soul: soul}
	}
}
