package kpi

import (
	"runtime"
	"time"
)

// Capture brackets a measured region and reports its allocation, RSS, and
// wall-clock deltas. Use it as:
//
//	c := kpi.NewCapture()
//	c.Begin()
//	... run the scenario ...
//	m := c.End()
//
// Begin runs a GC and snapshots the allocation counters so the captured allocs
// exclude prior setup garbage; End runs a final GC and reads the deltas. The RSS
// sampler runs between Begin and End. The numbers are process-wide (Go's
// MemStats and /proc are per-process), so a Capture must wrap a single scenario
// run with no concurrent benchmark in the same process — the harness runs
// scenarios sequentially, which holds.
type Capture struct {
	sampler   *RSSSampler
	startTime time.Time

	startMallocs    uint64
	startTotalAlloc uint64
}

// Metrics is the result of one Capture bracket.
type Metrics struct {
	// Allocs is the number of heap allocations during the measured region.
	Allocs uint64
	// Bytes is the cumulative bytes allocated during the measured region.
	Bytes uint64
	// RSSPeak is the highest sampled resident set size, in bytes (0 off-linux).
	RSSPeak uint64
	// RSSFinal is the resident set size at End, in bytes (0 off-linux).
	RSSFinal uint64
	// WallNs is the elapsed wall-clock time of the measured region, in ns.
	WallNs int64
}

// NewCapture returns a ready-to-Begin Capture.
func NewCapture() *Capture { return &Capture{} }

// Begin snapshots the allocation baseline (after a GC so prior garbage is not
// counted), starts the RSS sampler, and starts the wall clock.
func (c *Capture) Begin() {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c.startMallocs = ms.Mallocs
	c.startTotalAlloc = ms.TotalAlloc

	c.sampler = NewRSSSampler(10 * time.Millisecond)
	c.sampler.Start()
	c.startTime = time.Now()
}

// End stops the wall clock and the RSS sampler, runs a final GC, and returns the
// allocation/RSS/wall-clock deltas since Begin.
func (c *Capture) End() Metrics {
	wallNs := time.Since(c.startTime).Nanoseconds()
	peak, final := c.sampler.Stop()

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return Metrics{
		Allocs:   ms.Mallocs - c.startMallocs,
		Bytes:    ms.TotalAlloc - c.startTotalAlloc,
		RSSPeak:  peak,
		RSSFinal: final,
		WallNs:   wallNs,
	}
}
