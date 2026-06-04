package client

import (
	"context"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The skills-inventory discovery surface: a plain client-owned struct mirroring
// the proto SkillInfo message, the unary RPC wrapper that maps proto → the
// struct, and the tea.Cmd constructor the ui's /skills panel calls. As with the
// MCP and slash-command surfaces, NO proto type leaks past this file — the ui
// renders purely from the structs and msgs below, and the mapping is exercised
// offline against a fake client.

// Skill is one discovered skill (proto SkillInfo, proto-free): its activation
// name and a short one-line description. Metadata only — discovery carries no
// body; activation is a run-path concern handled server-side by the Skill tool.
type Skill struct {
	Name        string
	Description string
}

// SkillsMsg carries a ListSkills result for the /skills panel. Err is set on
// failure; the panel surfaces it rather than silently degrading, mirroring the
// MCP panel's error handling.
type SkillsMsg struct {
	Skills []Skill
	Err    error
}

// ListSkills lists the resolved skills inventory (a startup snapshot server-side).
func (c *Client) ListSkills(ctx context.Context) ([]Skill, error) {
	resp, err := c.svc.ListSkills(ctx, &mecatlv1.ListSkillsRequest{})
	if err != nil {
		return nil, err
	}
	return mapSkills(resp.GetSkills()), nil
}

// mapSkills maps proto SkillInfos to the plain structs (nil-safe).
func mapSkills(in []*mecatlv1.SkillInfo) []Skill {
	out := make([]Skill, 0, len(in))
	for _, s := range in {
		out = append(out, Skill{Name: s.GetName(), Description: s.GetDescription()})
	}
	return out
}

// SkillLister is the subset of *Client the ui's /skills panel needs. Splitting
// it out keeps the ui injectable with a fake for offline tests; *Client
// satisfies it.
type SkillLister interface {
	ListSkills(ctx context.Context) ([]Skill, error)
}

// ListSkillsCmd fetches the skills inventory off the update goroutine; the
// result (success or error) arrives as a SkillsMsg.
func ListSkillsCmd(ctx context.Context, c SkillLister) tea.Cmd {
	return func() tea.Msg {
		skills, err := c.ListSkills(ctx)
		if err != nil {
			return SkillsMsg{Err: err}
		}
		return SkillsMsg{Skills: skills}
	}
}
