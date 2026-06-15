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

// TestOperatorPostureFromCLIHonoured: an OPERATOR-TIER (CLI explicit) posture: scalar
// is read and returned by OperatorPosture(). Mirrors the guardrails operator-tier test.
func TestOperatorPostureFromCLIHonoured(t *testing.T) {
	const yaml = "posture: auto\n"
	env := envWithExplicit("/etc/mecatl/posture.yaml", yaml)
	r := newWithEnv(Options{ExplicitFiles: []string{"/etc/mecatl/posture.yaml"}}, env)
	if r == nil {
		t.Fatal("resolver should be non-nil with an explicit file")
	}
	if got := r.OperatorPosture(); got != "auto" {
		t.Fatalf("operator-tier posture must be honoured from CLI/explicit; got %q", got)
	}
}

// TestProjectPostureIgnoredWithWarn is the FAIL-CLOSED CORE: a PROJECT-TIER posture:
// scalar must NEVER become the operator posture, and the resolver WARNs naming why (a
// project repo raising the automation posture is a security downgrade). This is the
// test that would catch a malicious repo's .mecatl/settings.yaml `posture: yolo` being
// honoured.
func TestProjectPostureIgnoredWithWarn(t *testing.T) {
	var buf bytes.Buffer
	diag := slogdiag.New(&buf, false, port.LevelDebug)

	ws := &countingWS{Workspace: memfs.NewWorkspace("/repo")}
	ws.seed(t, projectFileMecatl, "posture: yolo\n")

	r := newWithEnv(Options{Conventional: true, TrustProject: true, Diagnostics: diag}, fakeEnv())
	_ = r.Resolve(context.Background(), ws)

	if got := r.OperatorPosture(); got != "" {
		t.Fatalf("a PROJECT-tier posture must NOT become the operator posture; got %q", got)
	}
	log := buf.String()
	if !strings.Contains(log, "IGNORING a project-tier posture") {
		t.Fatalf("expected an ignore-WARN naming the project tier; got:\n%s", log)
	}
}

// TestPostureCLIOutranksUser: CLI out-ranks user-global (first-non-empty keeps CLI).
func TestPostureCLIOutranksUser(t *testing.T) {
	const cliYAML = "posture: trusted\n"
	const userYAML = "posture: yolo\n"
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
			case "/cli/posture.yaml":
				return []byte(cliYAML), nil
			case "/cfg/mecatl/settings.yaml":
				return []byte(userYAML), nil
			}
			return nil, errors.New("not found")
		},
	}
	r := newWithEnv(Options{Conventional: true, ExplicitFiles: []string{"/cli/posture.yaml"}}, env)
	if got := r.OperatorPosture(); got != "trusted" {
		t.Fatalf("CLI posture must out-rank user-global; got %q", got)
	}
}

// TestOperatorPostureNilResolver pins the nil-safe accessor (a typed-nil resolver
// returns "" not a panic), matching OperatorGuardrails.
func TestOperatorPostureNilResolver(t *testing.T) {
	var r *Resolver
	if got := r.OperatorPosture(); got != "" {
		t.Fatalf("nil resolver OperatorPosture = %q, want empty", got)
	}
}

// TestOperatorPostureTrustProjectIndependent pins the property the composition's
// transient-resolver-then-rebuild dance RELIES on: OperatorPosture() is read ONCE at
// construction from the user-global + CLI tiers (loadUserRules), never the lazy
// per-root project path that the TrustProject gate touches — so it returns the SAME
// value whether TrustProject is true or false. If a refactor ever routed the posture
// read through the trust-gated project path, the composition's pre-applyPosture
// transient resolver (built before TrustProject is raised) could read a different value
// than Build's final resolver — this guards against that.
func TestOperatorPostureTrustProjectIndependent(t *testing.T) {
	const yaml = "posture: auto\n"
	for _, trust := range []bool{true, false} {
		env := envWithExplicit("/etc/mecatl/posture.yaml", yaml)
		r := newWithEnv(Options{ExplicitFiles: []string{"/etc/mecatl/posture.yaml"}, TrustProject: trust}, env)
		if got := r.OperatorPosture(); got != "auto" {
			t.Fatalf("OperatorPosture must be TrustProject-independent; TrustProject=%v gave %q, want \"auto\"", trust, got)
		}
	}
}
