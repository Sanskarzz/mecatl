package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/syscaller"
)

// --- the offline fake verifier ----------------------------------------------
//
// Token VALIDATION (signature, alg, iss/aud/exp/nbf) is delegated to the
// validator behind server.PrincipalValidator (ADR 0100 decision 3) and is NOT
// mecatl's code — so the tests here script the seam rather than mint real JWTs.
// What is under test is mecatl's EDGE behaviour: a verified principal reaches
// the handler context, a rejection never falls through to the handler, a
// transient IdP outage is not a 401, and the limiter keys on (iss, sub).
type fakeValidator struct {
	// ok maps a bearer string to the principal the validator vouches for.
	ok map[string]session.Principal
	// transient lists bearers whose validation fails because the JWKS endpoint
	// is unreachable (an IdP outage, not a bad token).
	transient map[string]bool
}

func (f fakeValidator) Validate(_ context.Context, bearer string) (*session.Principal, error) {
	if f.transient[bearer] {
		return nil, fmt.Errorf("fetch jwks: dial tcp: connection refused: %w", server.ErrIdentityUnavailable)
	}
	if p, okv := f.ok[bearer]; okv {
		return &p, nil
	}
	return nil, fmt.Errorf("token rejected: %w", server.ErrInvalidToken)
}

// edgeAlice is the principal the good tokens below vouch for. Identity is the
// (iss, sub) PAIR — the tests assert both.
var edgeAlice = session.Principal{
	Issuer:    "https://idp.example.com",
	Subject:   "alice",
	GrantType: session.GrantTypeUser,
	Name:      "Alice Example",
}

// oidcOnly builds an Authenticator with a wired verifier and NO static token —
// the OIDC deployment shape (AC1.4: identity does not ride authEnabled()).
func oidcOnly(t *testing.T, v fakeValidator, rate float64, burst int) *server.Authenticator {
	t.Helper()
	return server.NewAuthenticator(server.SecurityConfig{
		Validator: v,
		RateLimit: rate,
		RateBurst: burst,
	})
}

// callUnary drives the gRPC unary interceptor with an Authorization bearer (when
// non-empty) and reports the handler context it saw (nil when the handler never
// ran) plus the interceptor error.
func callUnary(a *server.Authenticator, bearer string) (handlerCtx context.Context, err error) {
	ctx := context.Background()
	if bearer != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+bearer))
	}
	_, err = a.UnaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/mecatl.v1.HarnessService/CreateSession"},
		func(c context.Context, _ any) (any, error) {
			handlerCtx = c
			return nil, nil
		})
	return handlerCtx, err
}

// fakeServerStream is the minimal grpc.ServerStream the StreamInterceptor needs:
// it only ever reads Context() before delegating, and principalStream wraps it.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeServerStream) Context() context.Context { return s.ctx }

// callStream drives the gRPC STREAM interceptor with an Authorization bearer
// (when non-empty) and reports the stream context the handler saw (nil when the
// handler never ran) plus the interceptor error. The StreamInterceptor has its
// own principalStream wrapping path, distinct from the unary one — so it needs
// its own coverage, not an inference from callUnary.
func callStream(a *server.Authenticator, bearer string) (handlerCtx context.Context, err error) {
	ctx := context.Background()
	if bearer != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+bearer))
	}
	err = a.StreamInterceptor()(nil, fakeServerStream{ctx: ctx},
		&grpc.StreamServerInfo{FullMethod: "/mecatl.v1.HarnessService/Converse"},
		func(_ any, ss grpc.ServerStream) error {
			handlerCtx = ss.Context()
			return nil
		})
	return handlerCtx, err
}

// callHTTP drives the HTTP middleware and reports the handler context it saw
// (nil when the handler never ran) plus the recorded response.
func callHTTP(a *server.Authenticator, bearer string) (handlerCtx context.Context, rec *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.RemoteAddr = "10.0.0.1:34567"
	rec = httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		handlerCtx = r.Context()
	})).ServeHTTP(rec, req)
	return handlerCtx, rec
}

// --- AC1.1 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_ValidTokenYieldsPrincipal pins AC1.1: with a
// verifier wired, a validated token yields the (iss, sub) principal on the
// handler's context, on BOTH surfaces.
func TestCallerIdentity_Scenario1_ValidTokenYieldsPrincipal(t *testing.T) {
	t.Parallel()

	// One bearer per signature algorithm the validator accepts. The algorithm is
	// the validator's business; what mecatl must do is identical for all three.
	v := fakeValidator{ok: map[string]session.Principal{
		"tok-rs256": edgeAlice,
		"tok-es256": edgeAlice,
		"tok-ps256": edgeAlice,
	}}
	auth := oidcOnly(t, v, 0, 0)

	for _, tok := range []string{"tok-rs256", "tok-es256", "tok-ps256"} {
		t.Run("grpc/"+tok, func(t *testing.T) {
			ctx, err := callUnary(auth, tok)
			if err != nil {
				t.Fatalf("interceptor err = %v, want nil", err)
			}
			if ctx == nil {
				t.Fatal("handler did not run")
			}
			got := session.PrincipalFromContext(ctx)
			if got == nil {
				t.Fatal("handler context carries no principal")
			}
			if *got != edgeAlice {
				t.Fatalf("principal = %+v, want %+v", *got, edgeAlice)
			}
		})
		t.Run("http/"+tok, func(t *testing.T) {
			ctx, rec := callHTTP(auth, tok)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ctx == nil {
				t.Fatal("handler did not run")
			}
			got := session.PrincipalFromContext(ctx)
			if got == nil || *got != edgeAlice {
				t.Fatalf("principal = %v, want %+v", got, edgeAlice)
			}
		})
	}
}

// --- AC1.2 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_BadTokensRejected pins AC1.2: every rejected
// token — wrong issuer/audience, expired, nbf in the future, bad signature,
// alg=none, HS*-confused — and a non-JWT are clean 401-class errors, and the
// handler NEVER runs (there is no fallback branch letting the request through).
func TestCallerIdentity_Scenario1_BadTokensRejected(t *testing.T) {
	t.Parallel()

	auth := oidcOnly(t, fakeValidator{ok: map[string]session.Principal{"good": edgeAlice}}, 0, 0)

	bad := []string{
		"wrong-issuer",
		"wrong-audience",
		"expired",
		"nbf-in-the-future",
		"bad-signature",
		"alg-none",
		"hs256-confused",
		"not.a.jwt at all", // malformed: a clean rejection, never a fallback
		"",                 // no Authorization header at all
	}
	for _, tok := range bad {
		t.Run("grpc/"+tok, func(t *testing.T) {
			ctx, err := callUnary(auth, tok)
			if code := status.Code(err); code != codes.Unauthenticated {
				t.Fatalf("code = %v, want Unauthenticated (err=%v)", code, err)
			}
			if ctx != nil {
				t.Fatal("handler RAN on a rejected token — fallback branch")
			}
		})
		t.Run("http/"+tok, func(t *testing.T) {
			ctx, rec := callHTTP(auth, tok)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if ctx != nil {
				t.Fatal("handler RAN on a rejected token — fallback branch")
			}
		})
	}
}

// --- AC1.3 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_NoAuthByteIdentical pins AC1.3: with no verifier
// wired (and no static token), a request with no credentials is processed
// unauthenticated — the handler runs and sees a NIL principal. No user is
// invented (the ToolHive anonymous-middleware anti-pattern ADR 0100 rejects),
// and the response carries no auth challenge: the default no-token path (the
// TUI's) is unchanged.
func TestCallerIdentity_Scenario1_NoAuthByteIdentical(t *testing.T) {
	t.Parallel()

	auth := server.NewAuthenticator(server.SecurityConfig{})

	ctx, err := callUnary(auth, "")
	if err != nil {
		t.Fatalf("interceptor err = %v, want nil", err)
	}
	if ctx == nil {
		t.Fatal("handler did not run")
	}
	if p := session.PrincipalFromContext(ctx); p != nil {
		t.Fatalf("principal = %+v, want nil (no user may be invented)", *p)
	}

	hctx, rec := callHTTP(auth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if hctx == nil {
		t.Fatal("handler did not run")
	}
	if p := session.PrincipalFromContext(hctx); p != nil {
		t.Fatalf("principal = %+v, want nil", *p)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want empty (no challenge on the no-auth path)", got)
	}

	// A bearer presented to a server with neither gate configured is ignored,
	// not validated into a principal.
	bctx, err := callUnary(auth, "whatever")
	if err != nil {
		t.Fatalf("interceptor err = %v, want nil", err)
	}
	if p := session.PrincipalFromContext(bctx); p != nil {
		t.Fatalf("principal = %+v, want nil", *p)
	}
}

// --- AC1.4 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_IdentityPredicateIndependent pins AC1.4: the OIDC
// enable predicate is whether a verifier is wired, NOT authEnabled(). A
// shared-token deployment has one credential and zero subjects; an OIDC
// deployment has subjects and no static token. Neither trips the other's gate.
func TestCallerIdentity_Scenario1_IdentityPredicateIndependent(t *testing.T) {
	t.Parallel()

	v := fakeValidator{ok: map[string]session.Principal{"good": edgeAlice}}

	t.Run("static token only: authenticated, zero subjects", func(t *testing.T) {
		auth := server.NewAuthenticator(server.SecurityConfig{AuthToken: "shared"})
		ctx, err := callUnary(auth, "shared")
		if err != nil {
			t.Fatalf("valid static token rejected: %v", err)
		}
		if p := session.PrincipalFromContext(ctx); p != nil {
			t.Fatalf("static-token deployment invented a principal: %+v", *p)
		}
		// The static gate still bites; identity being off does not open it.
		if _, err := callUnary(auth, "good"); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
		}
	})

	t.Run("verifier only: subjects, no static token", func(t *testing.T) {
		auth := oidcOnly(t, v, 0, 0)
		ctx, err := callUnary(auth, "good")
		if err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
		if p := session.PrincipalFromContext(ctx); p == nil || *p != edgeAlice {
			t.Fatalf("principal = %v, want %+v", p, edgeAlice)
		}
		// The identity gate bites with NO static token configured — proof it is
		// not gated on authEnabled().
		if _, err := callUnary(auth, ""); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("no-token code = %v, want Unauthenticated", status.Code(err))
		}
	})
}

// --- AC1.5 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_JWKSDownIsTransientNotUnauthorized pins AC1.5: a
// JWKS-unreachable condition is a transient 503-class signal, DISTINCT from a
// 401 bad token — an IdP outage must not be misread as an authn failure.
func TestCallerIdentity_Scenario1_JWKSDownIsTransientNotUnauthorized(t *testing.T) {
	t.Parallel()

	auth := oidcOnly(t, fakeValidator{
		ok:        map[string]session.Principal{"good": edgeAlice},
		transient: map[string]bool{"jwks-down": true},
	}, 0, 0)

	ctx, err := callUnary(auth, "jwks-down")
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", code, err)
	}
	if status.Code(err) == codes.Unauthenticated {
		t.Fatal("IdP outage reported as an authn failure")
	}
	if ctx != nil {
		t.Fatal("handler RAN while the IdP was unreachable")
	}

	hctx, rec := callHTTP(auth, "jwks-down")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if hctx != nil {
		t.Fatal("handler RAN while the IdP was unreachable")
	}

	// The distinction is real: a genuinely bad token on the SAME authenticator
	// is still a 401 / Unauthenticated.
	if _, err := callUnary(auth, "rubbish"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad-token code = %v, want Unauthenticated", status.Code(err))
	}
	if _, rec := callHTTP(auth, "rubbish"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401", rec.Code)
	}
}

// --- AC1.7 -------------------------------------------------------------------

// TestCallerIdentity_Scenario1_RateLimitKeyedOnPrincipal pins AC1.7: under OIDC
// the limiter keys on the validated (iss, sub), not the raw token — two
// different valid tokens for the same subject share one bucket, so rotating a
// token is not a limit bypass — and a REJECTED token is never keyed at all.
func TestCallerIdentity_Scenario1_RateLimitKeyedOnPrincipal(t *testing.T) {
	t.Parallel()

	v := fakeValidator{ok: map[string]session.Principal{
		"alice-old": edgeAlice,
		"alice-new": edgeAlice, // same subject, rotated credential
		"bob":       {Issuer: edgeAlice.Issuer, Subject: "bob", GrantType: session.GrantTypeUser},
	}}

	t.Run("rotation shares one bucket", func(t *testing.T) {
		auth := oidcOnly(t, v, 1, 1) // 1 rps, burst 1
		if _, err := callUnary(auth, "alice-old"); err != nil {
			t.Fatalf("first request err = %v, want nil", err)
		}
		_, err := callUnary(auth, "alice-new")
		if code := status.Code(err); code != codes.ResourceExhausted {
			t.Fatalf("rotated-token code = %v, want ResourceExhausted (token rotation bypassed the limit)", code)
		}

		keys := auth.TrackedClientKeysForTest()
		if len(keys) != 1 {
			t.Fatalf("tracked keys = %v, want exactly one (iss,sub) bucket", keys)
		}
		for _, k := range keys {
			for _, tok := range []string{"alice-old", "alice-new"} {
				if strings.Contains(k, tok) {
					t.Fatalf("limiter key %q embeds the raw token %q", k, tok)
				}
			}
			if !strings.Contains(k, edgeAlice.Subject) || !strings.Contains(k, edgeAlice.Issuer) {
				t.Fatalf("limiter key %q is not keyed on (iss, sub)", k)
			}
		}
	})

	t.Run("a different subject gets its own bucket", func(t *testing.T) {
		auth := oidcOnly(t, v, 1, 1)
		if _, err := callUnary(auth, "alice-old"); err != nil {
			t.Fatalf("edgeAlice err = %v, want nil", err)
		}
		// bob is over the GLOBAL budget here (burst 1), but he must still land in
		// his OWN bucket — two subjects never share one.
		_, _ = callUnary(auth, "bob")
		keys := auth.TrackedClientKeysForTest()
		if len(keys) != 2 {
			t.Fatalf("tracked keys = %v, want two distinct (iss,sub) buckets", keys)
		}
		if keys[0] == keys[1] {
			t.Fatalf("tracked keys = %v, want distinct", keys)
		}
	})

	t.Run("a rejected token is never keyed", func(t *testing.T) {
		auth := oidcOnly(t, fakeValidator{
			ok:        map[string]session.Principal{"good": edgeAlice},
			transient: map[string]bool{"jwks-down": true},
		}, 1, 1)
		for _, tok := range []string{"nope", "also-nope", "jwks-down", ""} {
			if _, err := callUnary(auth, tok); err == nil {
				t.Fatalf("token %q was accepted", tok)
			}
		}
		if keys := auth.TrackedClientKeysForTest(); len(keys) != 0 {
			t.Fatalf("tracked keys = %v, want none (a rejected token must not create a bucket)", keys)
		}
		// Nor may it have consumed the good subject's budget.
		if _, err := callUnary(auth, "good"); err != nil {
			t.Fatalf("valid request after rejections err = %v, want nil", err)
		}
		if keys := auth.TrackedClientKeysForTest(); !slices.ContainsFunc(keys, func(k string) bool {
			return strings.Contains(k, edgeAlice.Subject)
		}) {
			t.Fatalf("tracked keys = %v, want edgeAlice's bucket", keys)
		}
	})
}

// TestCallerIdentityStreamInterceptor covers the STREAM half of the gRPC edge.
// StreamInterceptor owns its own principalStream wrapping — it overrides the
// stream's Context so the handler sees the verified principal, and hands the
// handler the ORIGINAL stream when identity is off. None of that is exercised by
// the unary interceptor, so a regression there would ship silently: a stream RPC
// (Converse, the main prompt surface) would run with no identity at all.
func TestCallerIdentityStreamInterceptor(t *testing.T) {
	t.Parallel()

	v := fakeValidator{
		ok:        map[string]session.Principal{"good": edgeAlice},
		transient: map[string]bool{"jwks-down": true},
	}

	t.Run("valid token reaches the handler stream", func(t *testing.T) {
		ctx, err := callStream(oidcOnly(t, v, 0, 0), "good")
		if err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		if ctx == nil {
			t.Fatal("handler did not run")
		}
		got := session.PrincipalFromContext(ctx)
		if got == nil {
			t.Fatal("handler stream context carries no principal (principalStream did not wrap)")
		}
		if *got != edgeAlice {
			t.Fatalf("principal = %+v, want %+v", *got, edgeAlice)
		}
	})

	t.Run("bad token never reaches the handler", func(t *testing.T) {
		auth := oidcOnly(t, v, 0, 0)
		for _, tok := range []string{"rubbish", "not.a.jwt at all", ""} {
			ctx, err := callStream(auth, tok)
			if code := status.Code(err); code != codes.Unauthenticated {
				t.Errorf("token %q: code = %v, want Unauthenticated", tok, code)
			}
			if ctx != nil {
				t.Errorf("token %q: handler RAN on a rejected token", tok)
			}
		}
		// A transient IdP outage stays a 503-class Unavailable on streams too.
		ctx, err := callStream(auth, "jwks-down")
		if code := status.Code(err); code != codes.Unavailable {
			t.Errorf("code = %v, want Unavailable", code)
		}
		if ctx != nil {
			t.Error("handler RAN while the IdP was unreachable")
		}
	})

	t.Run("no identity configured: handler runs with a nil principal", func(t *testing.T) {
		ctx, err := callStream(server.NewAuthenticator(server.SecurityConfig{}), "")
		if err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		if ctx == nil {
			t.Fatal("handler did not run")
		}
		if p := session.PrincipalFromContext(ctx); p != nil {
			t.Fatalf("principal = %+v, want nil (no user may be invented)", *p)
		}
	})
}

// TestCallerIdentityEdgeRejectsMalformedPrincipal pins the edge's own guard on
// the validator's verdict (AC1.2): a non-nil principal is not automatically
// trustworthy. A principal missing either half of its (iss, sub) identity, one
// carrying a grant type outside the closed enum, and one presenting the INTERNAL
// namespace (mecatl:internal, or the system grant) are all 401-class rejections.
//
// Why each matters: an empty principal is exactly the fabricated-anonymous
// caller ADR 0100 decision 2 rejects, arriving through the front door; and an
// external token presenting the internal issuer is byte-identical to a system
// caller at every downstream consumer — including the isolation track (#368),
// which will read it to make decisions.
func TestCallerIdentityEdgeRejectsMalformedPrincipal(t *testing.T) {
	t.Parallel()

	bad := map[string]session.Principal{
		"no issuer":           {Subject: "alice", GrantType: session.GrantTypeUser},
		"no subject":          {Issuer: edgeAlice.Issuer, GrantType: session.GrantTypeUser},
		"wholly empty":        {},
		"unset grant type":    {Issuer: edgeAlice.Issuer, Subject: "alice"},
		"bogus grant type":    {Issuer: edgeAlice.Issuer, Subject: "alice", GrantType: session.GrantType("anonymous")},
		"internal issuer":     {Issuer: syscaller.Issuer, Subject: "childgc", GrantType: session.GrantTypeUser},
		"system grant":        {Issuer: edgeAlice.Issuer, Subject: "alice", GrantType: session.GrantTypeSystem},
		"internal issuer sys": {Issuer: syscaller.Issuer, Subject: "scheduler", GrantType: session.GrantTypeSystem},
	}

	v := fakeValidator{ok: map[string]session.Principal{"good": edgeAlice}}
	for name, p := range bad {
		v.ok[name] = p
	}

	for name := range bad {
		t.Run("grpc/"+name, func(t *testing.T) {
			auth := oidcOnly(t, v, 1, 1)
			ctx, err := callUnary(auth, name)
			if code := status.Code(err); code != codes.Unauthenticated {
				t.Fatalf("code = %v, want Unauthenticated (401-class, never a 503) (err=%v)", code, err)
			}
			if ctx != nil {
				t.Fatal("handler RAN with a malformed principal")
			}
			if keys := auth.TrackedClientKeysForTest(); len(keys) != 0 {
				t.Fatalf("tracked keys = %v, want none (a rejected principal must not create a bucket)", keys)
			}
		})
		t.Run("http/"+name, func(t *testing.T) {
			auth := oidcOnly(t, v, 1, 1)
			ctx, rec := callHTTP(auth, name)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if ctx != nil {
				t.Fatal("handler RAN with a malformed principal")
			}
			if keys := auth.TrackedClientKeysForTest(); len(keys) != 0 {
				t.Fatalf("tracked keys = %v, want none", keys)
			}
		})
	}

	// The guard is narrow: a well-formed principal on the SAME authenticator is
	// still accepted, so the rejections above are not a blanket deny.
	auth := oidcOnly(t, v, 0, 0)
	ctx, err := callUnary(auth, "good")
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if p := session.PrincipalFromContext(ctx); p == nil || *p != edgeAlice {
		t.Fatalf("principal = %v, want %+v", p, edgeAlice)
	}
}

// TestCallerIdentityErrorSentinels pins the small error taxonomy the edge maps
// on: a bad token and an unreachable IdP are distinct, errors.Is-able sentinels
// (a validator returning a wrapped sentinel must classify correctly).
func TestCallerIdentityErrorSentinels(t *testing.T) {
	t.Parallel()
	if errors.Is(server.ErrInvalidToken, server.ErrIdentityUnavailable) ||
		errors.Is(server.ErrIdentityUnavailable, server.ErrInvalidToken) {
		t.Fatal("the bad-token and IdP-outage sentinels must be distinct")
	}
}
