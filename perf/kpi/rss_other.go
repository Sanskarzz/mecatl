//go:build !linux

package kpi

// readRSS returns 0 on non-linux platforms: the harness's RSS KPI is Linux-only
// (CI + dev are Linux, per perf-tracking.md). A 0 reading is treated as "no
// sample" by the RSSSampler, so an off-linux run simply reports rss_peak_bytes
// and rss_final_bytes as 0 rather than failing.
func readRSS() uint64 { return 0 }
