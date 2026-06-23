package app

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/modelhook"
)

// TestLogGuardrailsPostureBranches pins the #159 build-once posture line: exactly ONE
// INFO per Build, one case per UX-B branch, with honest substrings (OFF no-config +
// hint, OFF kill-switch, ON via slot defaults, ON via slot superseding gate, ON via
// gate no slot, ON with custom rules + maxChecks). Each case asserts exactly one line.
func TestLogGuardrailsPostureBranches(t *testing.T) {
	cases := []struct {
		name      string
		cfg       Config
		wantCount int
		wantSubs  []string
		notSubs   []string
	}{
		{
			name:      "OFF no config + hint",
			cfg:       Config{},
			wantCount: 1,
			wantSubs:  []string{"guardrails: OFF", "no checker model configured", "guardrail", "--guardrails-model"},
		},
		{
			name:      "OFF kill-switch",
			cfg:       Config{GuardrailsDisabled: true, GuardrailsModel: "gpt-5-mini"},
			wantCount: 1,
			wantSubs:  []string{"guardrails: OFF", "kill-switch"},
		},
		{
			name: "ON via slot defaults (advisory + default set)",
			cfg: Config{
				UseMock:      true,
				ModelSlots:   map[string]string{slotGuardrail: "cheap"},
				ModelAliases: map[string]string{"cheap": "slot-id"},
			},
			wantCount: 1,
			wantSubs:  []string{"guardrails: ON", "checker=slot-id", "via slot `guardrail`", "mode=advisory", "default set"},
		},
		{
			name: "ON via slot superseding gate",
			cfg: Config{
				UseMock:         true,
				GuardrailsModel: "gate-id",
				ModelSlots:      map[string]string{slotGuardrail: "cheap"},
				ModelAliases:    map[string]string{"cheap": "slot-id"},
			},
			wantCount: 1,
			wantSubs:  []string{"guardrails: ON", "checker=slot-id", "supersedes gate value", "via slot `guardrail`"},
			notSubs:   []string{"checker=gate-id"},
		},
		{
			name: "ON via gate no slot",
			cfg: Config{
				UseMock:         true,
				GuardrailsModel: "gpt-5-mini",
			},
			wantCount: 1,
			wantSubs:  []string{"guardrails: ON", "checker=gpt-5-mini", "via --guardrails-model", "mode=advisory", "default set"},
		},
		{
			name: "ON with custom rules (block + rules + maxChecks)",
			cfg: Config{
				UseMock:             true,
				GuardrailsModel:     "gpt-5-mini",
				GuardrailsMaxChecks: 50,
				GuardrailsRules: []GuardrailRule{
					{Match: "WebFetch", Phases: []string{"post"}, Mode: string(modelhook.ModeBlock)},
				},
			},
			wantCount: 1,
			wantSubs:  []string{"guardrails: ON", "checker=gpt-5-mini", "mode=block", "rules=1", "maxChecks=50"},
			notSubs:   []string{"default set"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diag := &capturingDiag{}
			c.cfg.Diagnostics = diag
			logGuardrailsPosture(c.cfg)
			if got := len(diag.lines); got != c.wantCount {
				t.Fatalf("lines = %d, want %d; lines=%v", got, c.wantCount, diag.lines)
			}
			for _, sub := range c.wantSubs {
				if !diag.has(sub) {
					t.Fatalf("missing substring %q; lines=%v", sub, diag.lines)
				}
			}
			for _, sub := range c.notSubs {
				if diag.has(sub) {
					t.Fatalf("unexpected substring %q; lines=%v", sub, diag.lines)
				}
			}
		})
	}
}

// TestLogGuardrailsPostureReportsResolvedModelNotGate is the headline must-fix #1: a
// bound `guardrail` slot SUPERSEDES a differing gate value, and the ON posture line's
// `checker=` MUST be the SLOT model — NOT the (inert) gate value the old
// normalizeGuardrailsModel "guardrails ACTIVE" emit reported.
func TestLogGuardrailsPostureReportsResolvedModelNotGate(t *testing.T) {
	diag := &capturingDiag{}
	cfg := Config{
		UseMock:         true,
		GuardrailsModel: "gate-id",
		ModelSlots:      map[string]string{slotGuardrail: "cheap"},
		ModelAliases:    map[string]string{"cheap": "slot-id"},
		Diagnostics:     diag,
	}
	logGuardrailsPosture(cfg)
	if got := len(diag.lines); got != 1 {
		t.Fatalf("expected exactly one posture line, got %d; lines=%v", got, diag.lines)
	}
	line := diag.lines[0]
	if !strings.Contains(line, "checker=slot-id") {
		t.Fatalf("posture line must report the RESOLVED slot model, not the gate value; line=%q", line)
	}
	if strings.Contains(line, "checker=gate-id") {
		t.Fatalf("posture line must NOT report the inert gate value as the checker; line=%q", line)
	}
}
