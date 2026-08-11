package cliconfig

import (
	"context"
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

func TestOIDCMaxJWKSStalenessFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "default", want: time.Hour},
		{name: "override", args: []string{"--oidc-max-jwks-staleness=15m"}, want: 15 * time.Minute},
		{name: "disabled", args: []string{"--oidc-max-jwks-staleness=0"}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			var cfg OIDCConfig
			RegisterOIDCFlags(fs, &cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.MaxJWKSStaleness != tc.want {
				t.Fatalf("MaxJWKSStaleness = %v, want %v", cfg.MaxJWKSStaleness, tc.want)
			}
		})
	}
}

func TestOIDCValidatorRejectsNegativeMaxJWKSStaleness(t *testing.T) {
	_, err := OIDCValidator(context.Background(), OIDCConfig{MaxJWKSStaleness: -time.Second})
	if !errors.Is(err, ErrOIDCMisconfigured) {
		t.Fatalf("OIDCValidator error = %v, want ErrOIDCMisconfigured", err)
	}
}
