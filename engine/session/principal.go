package session

import "errors"

// GrantType names how a Principal was authenticated. It is a closed enum of
// exactly three values (ADR 0100 decision 1); the zero value is deliberately
// NOT a member, so an unset grant type is never mistaken for a valid one.
type GrantType string

const (
	// GrantTypeUser is an interactive end user (an OIDC authorization-code flow).
	GrantTypeUser GrantType = "user"
	// GrantTypeClientCredentials is a machine caller (an OAuth2
	// client-credentials flow), e.g. a schedule firing under its captured owner.
	GrantTypeClientCredentials GrantType = "client_credentials"
	// GrantTypeSystem is an internal harness goroutine (childgc, the memory/dream
	// consolidators, the scheduler tick/fire/delivery/reconcile). It is explicit
	// so an internal caller is never an ABSENT principal.
	GrantTypeSystem GrantType = "system"
)

// Valid reports whether g is one of the three defined grant types.
func (g GrantType) Valid() bool {
	switch g {
	case GrantTypeUser, GrantTypeClientCredentials, GrantTypeSystem:
		return true
	default:
		return false
	}
}

// Principal is the verified caller a session or schedule is attributed to
// (ADR 0100 decision 1). Identity is the (Issuer, Subject) PAIR, never Subject
// alone — two IdPs or realms collide on `sub`.
//
// It is a pure value object: comparable, stdlib-only, and deliberately narrow.
// It carries NO scopes, NO authority, NO credentials, and NO claims map — those
// belong to the enforcement tracks, not to attribution. It is modeled on
// ToolHive's PrincipalInfo; ToolHive is not imported.
//
// Absent identity is a nil *Principal, NEVER a fabricated anonymous one (the
// ToolHive anonymous-middleware anti-pattern ADR 0100 rejects).
type Principal struct {
	// Issuer is the IdP that minted the token (the canonical `iss` claim).
	Issuer string
	// Subject is the caller id within that issuer (the `sub` claim).
	Subject string
	// GrantType is how the caller authenticated.
	GrantType GrantType
	// Name is an optional human-readable display label. It is never part of
	// identity — display only.
	Name string
}

// Clone returns a copy of p, or nil when p is nil. It is the ONE place the
// "copy a *Principal across a boundary, nil stays nil" rule lives — every site
// that hands a principal out of, or into, a structure it does not own routes
// through it instead of hand-rolling the nil check and the deref.
//
// Principal is all-strings today, so a shallow copy IS a deep copy. The method
// exists so that the day it gains a slice or map field, every site stays correct
// together rather than silently becoming an aliasing bug.
func (p *Principal) Clone() *Principal {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// Authority is Track C's placeholder label on the Session aggregate. It is
// INERT in the caller-identity plan: nothing reads or writes it beyond the
// snapshot round-trip. It ships now so the contended engine/api/*.txt
// regeneration and CHANGELOG note are paid once (ADR 0100 consequences).
// The zero value ("") means "unset".
type Authority string

// ErrOwnerAlreadySet is returned by RestoreLabels when the session already
// carries a DIFFERENT owner. The owner is write-once (ADR 0100 decision 4).
var ErrOwnerAlreadySet = errors.New("session: owner already set")

// RestoreLabels stamps the write-once identity labels (Owner, Authority) on the
// aggregate. It is the restore seam sessnap uses — Session is an aggregate, so
// an adapter must not poke the exported fields.
//
// Write-once: a nil owner leaves the label unset (the ownerless / no-auth path,
// and it does NOT burn the slot); re-stamping the SAME owner value is
// idempotent; stamping a DIFFERENT owner over a set one returns
// ErrOwnerAlreadySet rather than silently re-owning the session. The owner is
// stored as a COPY, so the caller cannot mutate a stamped session's owner
// through its own pointer. A zero Authority leaves that label untouched (the
// same additive posture); it is otherwise inert in this plan.
func (s *Session) RestoreLabels(owner *Principal, authority Authority) error {
	if owner != nil {
		if s.Owner != nil && *s.Owner != *owner {
			return ErrOwnerAlreadySet
		}
		s.Owner = owner.Clone()
	}
	if authority != "" {
		s.Authority = authority
	}
	return nil
}
