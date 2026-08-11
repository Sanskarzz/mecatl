package app

// Test seams for the EXTERNAL app_test package (which must import
// internal/cliconfig — that package imports app, so the pinning test cannot be
// an in-package test). Only the internal goroutine starters whose
// system-principal wrap is pinned by
// TestCallerIdentity_Scenario2_InternalGoroutinesRunAsSystem are exported here.
var (
	StartChildGCForTest                = startChildGC
	StartMemoryConsolidationForTest    = startMemoryConsolidation
	StartUserModelConsolidationForTest = startUserModelConsolidation
)
