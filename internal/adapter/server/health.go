package server

import (
	"net/http"
)

// ReadyFunc reports whether the server's dependencies (engine, store) are ready
// to serve traffic. It is consulted by the /readyz handler on every probe so
// readiness can change at runtime. A nil ReadyFunc is treated as always-ready.
type ReadyFunc func() bool

// HealthHandler serves the liveness and readiness probes. Liveness (/healthz)
// always returns 200 while the process is up; readiness (/readyz) returns 200
// only when the injected ReadyFunc reports ready, else 503. These endpoints
// carry no secrets and MUST be mounted outside the auth/rate-limit middleware so
// orchestrators can probe them without credentials.
type HealthHandler struct {
	ready ReadyFunc
}

// NewHealthHandler constructs a HealthHandler. A nil ready func means the server
// is considered ready as soon as the process is up.
func NewHealthHandler(ready ReadyFunc) *HealthHandler {
	if ready == nil {
		ready = func() bool { return true }
	}
	return &HealthHandler{ready: ready}
}

// Live handles GET /healthz: a pure liveness probe that is 200 whenever the
// process can serve HTTP at all.
func (*HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Ready handles GET /readyz: 200 when dependencies are ready, 503 otherwise.
func (h *HealthHandler) Ready(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !h.ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// RegisterHealth mounts the health endpoints on mux. They are intentionally
// registered on the outer mux (before auth) so they bypass authentication and
// rate limiting.
func (h *HealthHandler) RegisterHealth(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.Live)
	mux.HandleFunc("GET /readyz", h.Ready)
}
