package cliconfig

import (
	"testing"
	"time"
)

func TestAuthnConfigPropagatesMaxJWKSStaleness(t *testing.T) {
	got := authnConfig(OIDCConfig{
		Issuer:           "https://idp.example.com",
		Audience:         "mecatl",
		JWKSURI:          "https://idp.example.com/keys",
		MaxJWKSStaleness: 23 * time.Minute,
	})
	if got.MaxJWKSStaleness != 23*time.Minute {
		t.Fatalf("authn.Config.MaxJWKSStaleness = %v, want 23m", got.MaxJWKSStaleness)
	}
	if got.AllowAnyAudience || got.InsecureAllowHTTP || got.AllowPrivateIP {
		t.Fatalf("security protections changed: AllowAnyAudience=%v InsecureAllowHTTP=%v AllowPrivateIP=%v",
			got.AllowAnyAudience, got.InsecureAllowHTTP, got.AllowPrivateIP)
	}
	if len(got.Audiences) != 1 || got.Audiences[0] != "mecatl" {
		t.Fatalf("authn.Config.Audiences = %v, want [mecatl]", got.Audiences)
	}
}
