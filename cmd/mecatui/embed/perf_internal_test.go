package embed

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/internal/app"
)

// TestSetupPerfDisabledLeavesTelemetrySeamsNil pins the no-perf path (issue
// #47): with PerfConfig.Enabled false, setupPerf must return a zero perfState
// and mutate NOTHING on cfg — Sink, ToolCallRecorder, and MetricsRoleScoper all
// stay nil, so the main engine is unmetered and every child engine keeps the
// byte-identical pre-feature nil/nil telemetry shape.
func TestSetupPerfDisabledLeavesTelemetrySeamsNil(t *testing.T) {
	cfg := app.Config{Workspace: t.TempDir(), Model: "mock"}

	ps, err := setupPerf(context.Background(), PerfConfig{}, &cfg)
	if err != nil {
		t.Fatalf("setupPerf(disabled): %v", err)
	}
	if ps.adminSrv != nil || ps.adminAddr != "" || ps.stop != nil || ps.shutdown != nil {
		t.Errorf("perf-off setupPerf returned a non-zero perfState: %+v", ps)
	}
	if cfg.Sink != nil {
		t.Errorf("perf-off cfg.Sink = %T, want nil", cfg.Sink)
	}
	if cfg.ToolCallRecorder != nil {
		t.Errorf("perf-off cfg.ToolCallRecorder = %T, want nil", cfg.ToolCallRecorder)
	}
	if cfg.MetricsRoleScoper != nil {
		t.Error("perf-off cfg.MetricsRoleScoper is non-nil; children must stay on the unmetered nil/nil path")
	}
}
