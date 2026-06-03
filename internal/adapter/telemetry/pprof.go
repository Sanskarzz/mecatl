package telemetry

import (
	"net/http"
	"net/http/pprof"
)

// pprofNamedProfiles are the runtime/pprof named profiles served via
// pprof.Handler(name) under /debug/pprof/<name>. They are the predefined
// profiles the runtime maintains; mutex and block only carry data once their
// sampling is armed (see the --mutex-profile-fraction / --block-profile-rate
// knobs in cmd/mecated). threadcreate is included for completeness even though
// it is rarely actionable.
var pprofNamedProfiles = []string{
	"heap",
	"goroutine",
	"allocs",
	"mutex",
	"block",
	"threadcreate",
}

// RegisterPprof registers the standard net/http/pprof handlers EXPLICITLY on the
// given mux. It deliberately does NOT rely on the package's init-time blank
// import (which mutates http.DefaultServeMux); registering on a caller-supplied
// mux keeps the profiling surface confined to the loopback admin listener and
// off the public service mux.
//
// It mounts the four dynamic endpoints (/debug/pprof/ Index, cmdline, profile,
// symbol, trace) plus one /debug/pprof/<name> handler per predefined profile
// (heap, goroutine, allocs, mutex, block, threadcreate) via pprof.Handler.
//
// SECURITY: pprof output (goroutine dumps, heap, the CPU/trace profiles) can
// embed prompt text, file paths, and other request data. Mount this ONLY on a
// loopback-bound listener — never on the public gRPC/HTTP service surface.
func RegisterPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range pprofNamedProfiles {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
}
