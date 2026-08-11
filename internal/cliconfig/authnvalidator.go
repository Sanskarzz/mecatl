package cliconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/stacklok/toolhive-core/authn"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// authnValidator adapts *authn.Validator to server.PrincipalValidator.
//
// It is the whole of mecatl's coupling to the token library: two type
// translations and an error mapping. Everything security-relevant about JWT
// verification — signature, alg, iss/aud/exp/nbf, JWKS fetch and rotation — is
// the library's, and mecatl hand-rolls none of it (ADR 0100 decision 3).
type authnValidator struct{ v *authn.Validator }

// Validate verifies the bearer and translates the result into mecatl's domain.
//
// Three translations, each load-bearing:
//
//   - authn.Principal carries NO GrantType (it has Issuer/Subject/Name/Claims),
//     while session.Principal requires one and the edge's admissiblePrincipal
//     REJECTS an out-of-enum grant. A field copy alone would therefore produce ""
//     and 401 every valid token; the grant is derived from the verified claim set
//     by server.GrantTypeFromClaims.
//   - the library returns a VALUE; mecatl's seam returns a pointer. The address is
//     taken of a local copy, never of anything the library retains.
//   - the library's failures are a *authn.Error carrying a Code, not errors.Is-able
//     sentinels, so the code is mapped onto mecatl's two (see errToSentinel).
func (a authnValidator) Validate(ctx context.Context, bearer string) (*session.Principal, error) {
	p, err := a.v.Validate(ctx, bearer)
	if err != nil {
		return nil, errToSentinel(err)
	}
	out := session.Principal{
		Issuer:    p.Issuer,
		Subject:   p.Subject,
		Name:      p.Name,
		GrantType: server.GrantTypeFromClaims(p.Claims),
	}
	return &out, nil
}

// Close stops the validator's background JWKS refresh. server.Authenticator.Close
// type-asserts io.Closer and calls this at shutdown — cancelling the server-root
// context does NOT stop the refresh loop (ADR 0027 List 1 row 39).
func (a authnValidator) Close() error { a.v.Close(); return nil }

// errToSentinel maps the library's error CODE onto mecatl's two sentinels.
//
//	CodeUnavailable    (503) → ErrIdentityUnavailable — key material unreachable,
//	                           i.e. an IdP OUTAGE. Distinct from a bad token so an
//	                           outage is never reported as an authn failure (AC1.5).
//	CodeInvalidToken   (401) → ErrInvalidToken
//	CodeInvalidRequest (400) → ErrInvalidToken
//
// Context cancellation and deadlines pass through unchanged: they describe the
// caller's request, not a JWT verdict, and the server maps them to their native
// gRPC status without spending rejected-token budget.
//
// Anything else unrecognised — including a non-*authn.Error — maps to
// ErrInvalidToken: fail CLOSED.
func errToSentinel(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var aerr *authn.Error
	if errors.As(err, &aerr) {
		switch aerr.Code {
		case authn.CodeUnavailable:
			return fmt.Errorf("%w: %s", server.ErrIdentityUnavailable, aerr.Reason)
		case authn.CodeInvalidToken, authn.CodeInvalidRequest:
			return fmt.Errorf("%w: %s", server.ErrInvalidToken, aerr.Reason)
		}
	}
	return fmt.Errorf("%w: unrecognised validator failure", server.ErrInvalidToken)
}

// defaultNewValidator is the production OIDCConfig.NewValidator: it builds the
// library's Config from the caller-identity flags while preserving mecatl's
// security policy (see the NewValidator doc comment).
//
// httpClient is a TEST seam and unexported for that reason. The library's
// AllowPrivateIP defaults to false — which is what stops a jwks_uri resolving to
// cloud instance metadata — and it applies only to the library's OWN client, so a
// test reaching an httptest server on 127.0.0.1 supplies its own client rather
// than relaxing a production default. Nothing in a real deployment sets it.
func defaultNewValidator(ctx context.Context, c OIDCConfig) (server.PrincipalValidator, error) {
	cfg := authnConfig(c)
	v, err := authn.NewValidator(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return authnValidator{v: v}, nil
}

func authnConfig(c OIDCConfig) authn.Config {
	cfg := authn.Config{
		Issuer:           c.Issuer,
		Audiences:        []string{c.Audience},
		JWKSURL:          c.JWKSURI,
		MaxJWKSStaleness: c.MaxJWKSStaleness,
		AllowAnyAudience: false,
		// Both default false, and only the explicitly-named test flag moves them.
		// InsecureAllowHTTP is a scheme check on the issuer URL; AllowPrivateIP is
		// an address check at dial time (re-applied per redirect hop). The e2e
		// suite needs BOTH because its IdP is a plaintext JWKS pod at an
		// in-cluster address — one without the other still refuses it.
		InsecureAllowHTTP: c.InsecureAllowPrivateIssuer,
		AllowPrivateIP:    c.InsecureAllowPrivateIssuer,
	}
	if c.httpClient != nil {
		cfg.HTTPClient = c.httpClient
	}
	return cfg
}
