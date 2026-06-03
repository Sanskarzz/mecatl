//go:build linux

package telemetry

import "github.com/prometheus/procfs"

// readRSS reads this process's resident set size in bytes from /proc via
// procfs: procfs.Self() → Proc.Stat() → ResidentMemory() (RSS pages ×
// pagesize). It returns 0 on any error (procfs unavailable, parse failure) so
// the caller treats RSS as simply unknown rather than failing.
//
// This is the Linux build of readRSS; see process_rss_other.go for the
// non-Linux stub. The split is a build tag rather than a runtime GOOS check so
// the procfs import is confined to Linux builds.
func readRSS() uint64 {
	proc, err := procfs.Self()
	if err != nil {
		return 0
	}
	stat, err := proc.Stat()
	if err != nil {
		return 0
	}
	rss := stat.ResidentMemory()
	if rss < 0 {
		return 0
	}
	return uint64(rss)
}

// rssSupported reports whether RSS reading is available on this platform. On
// Linux it is; the gauge registration in metrics.go consults this so the
// mecatl.process.rss series is simply absent off Linux.
func rssSupported() bool { return true }
