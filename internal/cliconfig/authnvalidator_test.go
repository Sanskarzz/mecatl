package cliconfig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// --- the static-JWKS fixture -------------------------------------------------

const (
	testKID      = "test-key-1"
	testAudience = "mecatl"
	testSubject  = "alice"
)

// jwksFixture is an httptest TLS server serving ONE RSA public key as a JWKS, plus
// the signer for tokens it vouches for.
//
// Tokens are signed IN-TEST rather than read from a fixture file, deliberately:
// a committed token carries a fixed `exp` and the suite would start failing on a
// calendar date with no code change.
type jwksFixture struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	discovery *atomic.Int64 // hits on the discovery document
	jwks      *atomic.Int64 // hits on the JWKS endpoint
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	f := &jwksFixture{key: key, discovery: &atomic.Int64{}, jwks: &atomic.Int64{}}

	mux := http.NewServeMux()
	// The JWKS: one RSA key, hand-rolled rather than pulling a JOSE library in
	// just to marshal two big-endian integers.
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		f.jwks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": testKID,
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	// The discovery document. AC2.3 asserts this is NEVER hit when the JWKS URI
	// is pinned, so the counter is the whole point of serving it.
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		f.discovery.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   f.srv.URL,
			"jwks_uri": f.srv.URL + "/keys",
		})
	})
	// A TLS server, not a plain one: InsecureAllowHTTP is a CONFIG-level scheme
	// check on the issuer URL and fires regardless of which HTTP client is
	// supplied, so an http:// fixture would force relaxing a production default.
	// Serving over TLS keeps BOTH pinned defaults intact — the scheme check passes,
	// and f.srv.Client() carries the test CA so the supplied client (which is also
	// what bypasses the loopback AllowPrivateIP check) trusts it.
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// sign mints an RS256 token for the given issuer/audience/subject.
func (f *jwksFixture) sign(t *testing.T, iss, aud, sub string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": iss,
		"aud": aud,
		"sub": sub,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
	})
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// validatorFor builds the validator through the REAL production path
// (OIDCValidator → defaultNewValidator), so the assertions cover mecatl's own
// authn.Config construction rather than a config the test wrote itself. That is
// the entire point of scenario 2: a Config with an empty Audiences and
// AllowAnyAudience true would leave the library behaving correctly while mecatl
// accepted tokens minted for another service, and only the real path can catch it.
func (f *jwksFixture) validatorFor(t *testing.T, audience string) server.PrincipalValidator {
	t.Helper()
	v, err := OIDCValidator(context.Background(), OIDCConfig{
		Issuer:       f.srv.URL,
		Audience:     audience,
		JWKSURI:      f.srv.URL + "/keys",
		NewValidator: defaultNewValidator,
		// The library's AllowPrivateIP is false by default and blocks loopback;
		// it governs only the library's own client, so the test supplies one
		// rather than relaxing a production default.
		httpClient: f.srv.Client(),
	})
	if err != nil {
		t.Fatalf("OIDCValidator: %v", err)
	}
	if v == nil {
		t.Fatal("OIDCValidator returned no validator with an issuer configured")
	}
	if c, ok := v.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	return v
}

// --- AC2.1 -------------------------------------------------------------------

// TestCallerIdentityE2E_Scenario2_RealTokenYieldsPrincipal pins AC2.1: a
// genuinely-signed RS256 token from the configured issuer and audience yields a
// principal carrying the token's (iss, sub) AND a grant that satisfies Valid().
//
// The grant half is not incidental. authn.Principal has no GrantType; a
// field-copy adapter would produce "" which admissiblePrincipal rejects, so
// EVERY valid token would 401. This is the assertion that catches that.
func TestCallerIdentityE2E_Scenario2_RealTokenYieldsPrincipal(t *testing.T) {
	f := newJWKSFixture(t)
	v := f.validatorFor(t, testAudience)

	p, err := v.Validate(context.Background(), f.sign(t, f.srv.URL, testAudience, testSubject))
	if err != nil {
		t.Fatalf("a correctly-signed token was rejected: %v", err)
	}
	if p == nil {
		t.Fatal("validator returned a nil principal with a nil error")
	}
	if p.Issuer != f.srv.URL || p.Subject != testSubject {
		t.Fatalf("identity = (%q, %q), want (%q, %q)", p.Issuer, p.Subject, f.srv.URL, testSubject)
	}
	if !p.GrantType.Valid() {
		t.Fatalf("GrantType = %q, which is not Valid() — admissiblePrincipal would 401 every valid token", p.GrantType)
	}
	if p.GrantType == session.GrantTypeSystem {
		t.Fatalf("GrantType = %q: a token must never yield the in-process system grant", p.GrantType)
	}
}

// --- AC2.2 -------------------------------------------------------------------

// TestCallerIdentityE2E_Scenario2_WrongAudienceRejected pins AC2.2: the same
// signing key and issuer with a DIFFERENT audience is rejected.
//
// This is the insecure-but-green configuration the scenario exists for. If
// mecatl built authn.Config with an empty Audiences, or with AllowAnyAudience
// true, the library would happily accept a token minted for another service and
// every other test here would still pass.
func TestCallerIdentityE2E_Scenario2_WrongAudienceRejected(t *testing.T) {
	f := newJWKSFixture(t)
	v := f.validatorFor(t, testAudience)

	_, err := v.Validate(context.Background(), f.sign(t, f.srv.URL, "some-other-service", testSubject))
	if err == nil {
		t.Fatal("a token minted for a DIFFERENT audience was accepted — Audiences is empty or AllowAnyAudience is true")
	}
	if !errors.Is(err, server.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken (401-class), not a transient", err)
	}
	if errors.Is(err, server.ErrIdentityUnavailable) {
		t.Fatal("a wrong-audience token was reported as an IdP outage")
	}
}

// --- AC2.3 -------------------------------------------------------------------

// TestCallerIdentityE2E_Scenario2_StaticJWKSSkipsDiscovery pins AC2.3: with the
// JWKS URI pinned, OIDC discovery is never attempted — the air-gapped and
// offline path. The discovery document is served and counted, so a regression
// that started resolving it would be visible rather than merely slower.
func TestCallerIdentityE2E_Scenario2_StaticJWKSSkipsDiscovery(t *testing.T) {
	f := newJWKSFixture(t)
	v := f.validatorFor(t, testAudience)

	if _, err := v.Validate(context.Background(), f.sign(t, f.srv.URL, testAudience, testSubject)); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := f.discovery.Load(); got != 0 {
		t.Fatalf("discovery document fetched %d time(s) despite a pinned --oidc-jwks-uri", got)
	}
	if got := f.jwks.Load(); got == 0 {
		t.Fatal("the pinned JWKS endpoint was never fetched — the test proves nothing about where keys came from")
	}
}
