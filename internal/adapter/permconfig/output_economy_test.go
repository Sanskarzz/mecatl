package permconfig

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/internal/adapter/slogdiag"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// TestOperatorOutputEconomyFromCLIHonoured: an OPERATOR-TIER (CLI explicit)
// output-economy: scalar is read and returned by OperatorOutputEconomy(). Mirrors
// the posture operator-tier test (ADR 0041).
func TestOperatorOutputEconomyFromCLIHonoured(t *testing.T) {
	const yaml = "output-economy: terse\n"
	env := envWithExplicit("/etc/mecatl/economy.yaml", yaml)
	r := newWithEnv(Options{ExplicitFiles: []string{"/etc/mecatl/economy.yaml"}}, env)
	if r == nil {
		t.Fatal("resolver should be non-nil with an explicit file")
	}
	if got := r.OperatorOutputEconomy(); got != "terse" {
		t.Fatalf("operator-tier output-economy must be honoured from CLI/explicit; got %q", got)
	}
}

// TestProjectOutputEconomyIgnoredWithWarn: a PROJECT-TIER output-economy: scalar
// must NEVER become the operator value, and the resolver WARNs naming why
// (operator-tier only — ADR 0041, for consistency with posture/guardrails).
func TestProjectOutputEconomyIgnoredWithWarn(t *testing.T) {
	var buf bytes.Buffer
	diag := slogdiag.New(&buf, false, port.LevelDebug)

	ws := &countingWS{Workspace: memfs.NewWorkspace("/repo")}
	ws.seed(t, projectFileMecatl, "output-economy: terse\n")

	r := newWithEnv(Options{Conventional: true, TrustProject: true, Diagnostics: diag}, fakeEnv())
	_ = r.Resolve(context.Background(), ws)

	if got := r.OperatorOutputEconomy(); got != "" {
		t.Fatalf("a PROJECT-tier output-economy must NOT become the operator value; got %q", got)
	}
	log := buf.String()
	if !strings.Contains(log, "IGNORING a project-tier output-economy") {
		t.Fatalf("expected an ignore-WARN naming the project tier; got:\n%s", log)
	}
}

// TestOutputEconomyCLIOutranksUser: CLI out-ranks user-global (first-non-empty keeps CLI).
func TestOutputEconomyCLIOutranksUser(t *testing.T) {
	const cliYAML = "output-economy: normal\n"
	const userYAML = "output-economy: terse\n"
	env := xdgconfig.ResolveEnv{
		Getenv: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return "/cfg"
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return "/home/u", nil },
		ReadFile: func(p string) ([]byte, error) {
			switch p {
			case "/cli/economy.yaml":
				return []byte(cliYAML), nil
			case "/cfg/mecatl/settings.yaml":
				return []byte(userYAML), nil
			}
			return nil, errors.New("not found")
		},
	}
	r := newWithEnv(Options{Conventional: true, ExplicitFiles: []string{"/cli/economy.yaml"}}, env)
	if got := r.OperatorOutputEconomy(); got != "normal" {
		t.Fatalf("CLI output-economy must out-rank user-global; got %q", got)
	}
}

// TestOperatorOutputEconomyNilResolver pins the nil-safe accessor (a typed-nil
// resolver returns "" not a panic), matching OperatorPosture.
func TestOperatorOutputEconomyNilResolver(t *testing.T) {
	var r *Resolver
	if got := r.OperatorOutputEconomy(); got != "" {
		t.Fatalf("nil resolver OperatorOutputEconomy = %q, want empty", got)
	}
}
