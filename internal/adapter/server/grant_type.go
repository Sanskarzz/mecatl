package server

import (
	"strings"

	"github.com/stacklok/mecatl/engine/session"
)

// GrantTypeFromClaims derives a Principal's grant type from a verified token's
// claim set. It exists because the two halves of the seam disagree: the
// validator library (toolhive-core/authn) hands back an issuer/subject/name/claims
// principal with NO grant type, while session.Principal requires one and the
// edge's admissiblePrincipal REJECTS an out-of-enum grant — so a field-copy
// adapter would produce "" and 401 every valid token. The library offers no
// helper (it has no azp/cid/client_id/grant handling at all), so the derivation
// is mecatl's, and it lives here — next to the guard that consumes it, and
// exported so the adapter that will construct the validator can call it without
// re-deriving the rules.
//
// It reads claims, never the raw token: signature, alg, iss/aud/exp/nbf are the
// validator's job and are already done by the time this runs.
//
// SIGNALS, in order of authority:
//
//  1. An explicit grant claim — `grant_type` (RFC-shaped) or `gty` (Auth0's
//     spelling, e.g. `gty: "client-credentials"`). Only the two OAuth2 grants
//     mecatl models are recognised, matched case- and separator-insensitively; a
//     non-string or unrecognised value is IGNORED rather than trusted, and falls
//     through to (2). This is deliberately a closed match: it is the only place
//     an attacker-controlled string could name a grant, and an open mapping is
//     how a token would talk its way into a grant it was not issued for.
//  2. Client identity equal to subject — `azp`, `client_id` or `cid` (Okta)
//     matching a NON-EMPTY `sub`. A client-credentials token has no resource
//     owner, so the IdP puts the client's own id in `sub`; a user token's `sub`
//     is the human and differs from the client that requested it.
//
// DEFAULT (no signal): session.GrantTypeUser. Three reasons, since the default
// decides the common case — most authorization-code IdPs emit no grant claim at
// all:
//
//   - The value must be valid (an invalid one is a blanket 401) and must not be
//     the system grant, which leaves exactly user or client_credentials.
//   - "client_credentials" is the STRONGER claim: it asserts that no human is
//     behind the call, and downstream consumers may use that to skip
//     human-facing gates (approval prompts, per-user attribution and, later, the
//     isolation track). Asserting it without evidence would hand a real user's
//     token the unattended-machine posture. "user" is the weaker, conservative
//     claim: it keeps human-facing gates engaged.
//   - Empirically, a missing grant claim with a `sub` that is not the client id
//     is what an authorization-code token looks like.
//
// It NEVER returns session.GrantTypeSystem. That value is minted in-process by
// internal/syscaller only; deriving it from a token would make an external
// caller byte-identical to a harness goroutine at every consumer — the exact
// privilege confusion admissiblePrincipal refuses.
func GrantTypeFromClaims(claims map[string]any) session.GrantType {
	switch normalizeGrantClaim(claimString(claims, "grant_type"), claimString(claims, "gty")) {
	case "client_credentials":
		return session.GrantTypeClientCredentials
	case "authorization_code", "implicit", "password", "refresh_token", "device_code":
		return session.GrantTypeUser
	}
	if sub := claimString(claims, "sub"); sub != "" {
		for _, key := range []string{"azp", "client_id", "cid"} {
			if claimString(claims, key) == sub {
				return session.GrantTypeClientCredentials
			}
		}
	}
	return session.GrantTypeUser
}

// normalizeGrantClaim folds the spellings of an explicit grant claim onto the
// RFC 6749 snake_case names, taking the first non-empty of the candidates. It
// lower-cases, maps "-" to "_" (Auth0's "client-credentials") and drops a URN
// prefix (the device-code grant is "urn:ietf:params:oauth:grant-type:device_code").
// An unrecognised result is harmless: the caller's switch ignores it.
func normalizeGrantClaim(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		v := strings.ToLower(strings.TrimSpace(c))
		if i := strings.LastIndex(v, ":"); i >= 0 {
			v = v[i+1:]
		}
		return strings.ReplaceAll(v, "-", "_")
	}
	return ""
}

// claimString reads a claim as a string, returning "" when it is absent or not a
// string. A non-string claim is never coerced: a grant decision made from a
// number or an object would be a decision made from something the IdP did not
// say.
func claimString(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}
