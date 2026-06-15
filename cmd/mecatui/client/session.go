package client

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The session-refetch surface: a unary GetSession wrapper, the message the ui's
// footer context-meter heal consumes, the narrow getter interface, and the
// tea.Cmd constructor. As with the rest of this package, NO proto type leaks
// past this file — the ui consumes only ResolvedModel / ResolvedModelMsg.

// GetSession looks up an existing session by id and returns the EFFECTIVE
// provider+model the server has resolved it to (echoed verbatim, including the
// context window). This is the self-healing refetch the footer context meter uses:
// for a session on a LIVE-ONLY model (present in the live /models listing but not
// the curated catalog — e.g. an OpenRouter openai/gpt-5.5) the create-time echo can
// carry a 0 / curated-floor window when the async live model-list swap had not yet
// landed; once it has, the server's ResolvedModel resolves the real live window
// (live-first via the same windowResolver the engine compacts at) and GetSession
// returns it. A nil Session/ResolvedModel (older server) yields the zero value (see
// resolvedModelFrom).
func (c *Client) GetSession(ctx context.Context, id string) (ResolvedModel, error) {
	resp, err := c.svc.GetSession(ctx, &mecatlv1.GetSessionRequest{SessionId: id})
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("get session: %w", err)
	}
	return resolvedModelFrom(resp.GetSession().GetResolvedModel()), nil
}

// ResolvedModelMsg carries the result of a GetSession refetch (the footer
// context-meter heal, issue #66). SessionID is STAMPED on every result — success
// AND error — so the reducer can drop a result that landed AFTER a /models switch
// rebound the ui to a new session (a stale window must never clobber the new
// session's denominator). Err set ⇒ the refetch failed; the reducer keeps the
// current denominator (benign — the heal simply retries on the next turn boundary).
type ResolvedModelMsg struct {
	SessionID string
	Resolved  ResolvedModel
	Err       error
}

// SessionGetter is the narrow subset of *Client that RefreshResolvedModelCmd needs.
// Splitting it out keeps the ui injectable with a fake for offline tests; *Client
// (and the ui's wider SessionCreator, via the sessionAdapter) satisfies it.
type SessionGetter interface {
	GetSession(ctx context.Context, id string) (ResolvedModel, error)
}

// RefreshResolvedModelCmd refetches the resolved model for session id off the
// update goroutine; the result (success or error) arrives as a ResolvedModelMsg
// with SessionID stamped so the reducer can correlate/drop it. It backs the footer
// context-meter heal: the ui fires it on a turn boundary while the meter's
// denominator is still unknown, and the ResolvedModelMsg arm raises the window once
// the server's live-first resolution heals it.
func RefreshResolvedModelCmd(ctx context.Context, g SessionGetter, id string) tea.Cmd {
	return func() tea.Msg {
		rm, err := g.GetSession(ctx, id)
		return ResolvedModelMsg{SessionID: id, Resolved: rm, Err: err}
	}
}
