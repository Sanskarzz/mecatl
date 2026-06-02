//go:build race

package ui

// raceEnabled reports whether the test binary was built with the race detector
// (`go test -race`). The Go race detector serialises memory accesses and adds a
// documented 2–20× CPU/latency multiplier; combined with `task test`'s parallel
// `go test -race ./...` (every package's tests contending for the same cores),
// the Bubble Tea program's renderer + command goroutines get CPU-starved and a
// fixed wall-clock teatest WaitFor deadline can fire before the awaited frame is
// produced. The teatest cases scale their deadlines by raceWaitScale when this is
// true, so the suite is robust to that starvation WITHOUT widening the non-race
// timeouts (the diagnosis: the flake is harness wall-clock-vs-starved-renderer,
// not an application logic bug — see TestFullCycleProgram et al.).
const raceEnabled = true
