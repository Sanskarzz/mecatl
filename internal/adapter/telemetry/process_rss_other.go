//go:build !linux

package telemetry

// readRSS returns 0 on non-Linux platforms: the procfs RSS source is
// Linux-only, so RSS is reported as unknown. See process_rss_linux.go for the
// real implementation.
func readRSS() uint64 { return 0 }

// rssSupported reports false off Linux, so the mecatl.process.rss gauge is not
// registered.
func rssSupported() bool { return false }
