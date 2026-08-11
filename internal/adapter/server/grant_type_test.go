package server_test

import (
	"testing"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// TestGrantTypeFromClaims pins the claim→grant derivation the future
// toolhive-core/authn adapter depends on. authn.Principal carries NO grant type,
// session.Principal requires one, and admissiblePrincipal rejects an invalid
// one — so a derivation that returned "" would 401 every valid token, and one
// that could return GrantTypeSystem would be a privilege-confusion bug.
func TestGrantTypeFromClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		claims map[string]any
		want   session.GrantType
	}{
		{
			name:   "nil claims default to the conservative user grant",
			claims: nil,
			want:   session.GrantTypeUser,
		},
		{
			name:   "empty claims default to the conservative user grant",
			claims: map[string]any{},
			want:   session.GrantTypeUser,
		},
		{
			name: "a human authorization-code token is a user grant",
			claims: map[string]any{
				"sub": "auth0|ada", "azp": "mecatl-cli", "email": "ada@example.com",
			},
			want: session.GrantTypeUser,
		},
		{
			name:   "an explicit authorization_code grant_type claim is a user grant",
			claims: map[string]any{"sub": "ada", "grant_type": "authorization_code"},
			want:   session.GrantTypeUser,
		},
		{
			name:   "Auth0's gty client-credentials is a machine grant",
			claims: map[string]any{"sub": "svc@clients", "gty": "client-credentials"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "an explicit client_credentials grant_type claim is a machine grant",
			claims: map[string]any{"sub": "svc", "grant_type": "CLIENT_CREDENTIALS"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "azp equal to sub is a machine grant",
			claims: map[string]any{"sub": "svc-runner", "azp": "svc-runner"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "client_id equal to sub is a machine grant",
			claims: map[string]any{"sub": "svc-runner", "client_id": "svc-runner"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "Okta's cid equal to sub is a machine grant",
			claims: map[string]any{"sub": "0oaSvc", "cid": "0oaSvc"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "an empty sub cannot match an empty azp into a machine grant",
			claims: map[string]any{"sub": "", "azp": ""},
			want:   session.GrantTypeUser,
		},
		{
			name:   "a hostile grant_type=system claim never yields the system grant",
			claims: map[string]any{"sub": "attacker", "grant_type": "system"},
			want:   session.GrantTypeUser,
		},
		{
			name:   "a hostile gty=system claim never yields the system grant",
			claims: map[string]any{"sub": "attacker", "gty": "system", "azp": "attacker"},
			want:   session.GrantTypeClientCredentials,
		},
		{
			name:   "a non-string grant_type claim is ignored, not trusted",
			claims: map[string]any{"sub": "ada", "grant_type": 42},
			want:   session.GrantTypeUser,
		},
		{
			name:   "an unknown grant_type falls through to the sub-equality signals",
			claims: map[string]any{"sub": "svc", "grant_type": "urn:example:custom", "azp": "svc"},
			want:   session.GrantTypeClientCredentials,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := server.GrantTypeFromClaims(tc.claims)
			if got != tc.want {
				t.Fatalf("GrantTypeFromClaims(%v) = %q, want %q", tc.claims, got, tc.want)
			}
			// Every returned value must be admissible at the edge: an invalid
			// grant is a blanket 401, and the system grant is minted only
			// in-process by internal/syscaller.
			if !got.Valid() {
				t.Fatalf("GrantTypeFromClaims(%v) = %q, which is not a valid grant", tc.claims, got)
			}
			if got == session.GrantTypeSystem {
				t.Fatalf("GrantTypeFromClaims(%v) derived the SYSTEM grant from a token", tc.claims)
			}
		})
	}
}

// TestGrantTypeFromClaimsNeverDerivesSystem is the exhaustive half of the
// privilege-confusion guard: no spelling of a "system" claim, in any of the
// claims the derivation reads, may produce session.GrantTypeSystem.
func TestGrantTypeFromClaimsNeverDerivesSystem(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"grant_type", "gty", "azp", "client_id", "cid", "sub", "iss"} {
		for _, val := range []string{"system", "System", "SYSTEM", "mecatl:internal", "syscaller"} {
			claims := map[string]any{"sub": val, key: val}
			if got := server.GrantTypeFromClaims(claims); got == session.GrantTypeSystem {
				t.Fatalf("claims %v derived the SYSTEM grant", claims)
			}
		}
	}
}
