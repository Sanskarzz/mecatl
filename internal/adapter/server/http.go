package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/stacklok/ozzharness/internal/session"
)

// HTTPHandler is the HTTP/SSE adapter over the shared Service. It serves the
// thin REST surface from ARCHITECTURE §7.2:
//
//	POST /v1/sessions               -> CreateSession (JSON)
//	GET  /v1/sessions/{id}          -> GetSession (JSON snapshot)
//	POST /v1/sessions/{id}/prompt   -> start a run; text/event-stream of Events
//	POST /v1/sessions/{id}/approve  -> resolve the paused ask on the run
//	POST /v1/sessions/{id}/cancel   -> cancel the in-flight run
//
// Every Event is emitted as one SSE `data:` line carrying the proto Event
// marshalled to JSON, so the HTTP and gRPC surfaces share one event shape.
type HTTPHandler struct {
	svc *Service
	mux *http.ServeMux
}

// NewHTTPHandler constructs an HTTPHandler over svc. The returned value is an
// http.Handler ready to mount.
func NewHTTPHandler(svc *Service) *HTTPHandler {
	h := &HTTPHandler{svc: svc, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/sessions", h.createSession)
	h.mux.HandleFunc("GET /v1/sessions/{id}", h.getSession)
	h.mux.HandleFunc("POST /v1/sessions/{id}/prompt", h.prompt)
	h.mux.HandleFunc("POST /v1/sessions/{id}/approve", h.approve)
	h.mux.HandleFunc("POST /v1/sessions/{id}/cancel", h.cancel)
	return h
}

// ServeHTTP routes to the registered handlers.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// --- request/response bodies ------------------------------------------------

type createSessionBody struct {
	Workspace string    `json:"workspace"`
	Mode      string    `json:"mode,omitempty"`
	Limits    *limitsIn `json:"limits,omitempty"`
}

type limitsIn struct {
	MaxTurns               int `json:"max_turns,omitempty"`
	MaxToolCalls           int `json:"max_tool_calls,omitempty"`
	MaxConsecutiveFailures int `json:"max_consecutive_failures,omitempty"`
}

type createSessionResp struct {
	SessionID string `json:"session_id"`
}

type sessionResp struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Mode      string `json:"mode"`
	Workspace string `json:"workspace"`
	Turns     int    `json:"turns"`
	ToolCalls int    `json:"tool_calls"`
}

type promptBody struct {
	Text string `json:"text"`
}

type approveBody struct {
	AskID string `json:"ask_id"`
	Allow bool   `json:"allow"`
}

// --- handlers ---------------------------------------------------------------

// createSession handles POST /v1/sessions.
func (h *HTTPHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var body createSessionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Workspace == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}
	var limits session.Limits
	if body.Limits != nil {
		limits = session.Limits{
			MaxTurns:               body.Limits.MaxTurns,
			MaxToolCalls:           body.Limits.MaxToolCalls,
			MaxConsecutiveFailures: body.Limits.MaxConsecutiveFailures,
		}
	}
	sess, err := h.svc.CreateSession(r.Context(), body.Workspace, modeFromString(body.Mode), limits)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createSessionResp{SessionID: string(sess.ID)})
}

// getSession handles GET /v1/sessions/{id}.
func (h *HTTPHandler) getSession(w http.ResponseWriter, r *http.Request) {
	id := session.SessionID(r.PathValue("id"))
	sess, err := h.svc.GetSession(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResp{
		SessionID: string(sess.ID),
		State:     string(sess.State),
		Mode:      string(sess.Mode),
		Workspace: sess.Workspace,
		Turns:     sess.Counters.Turns,
		ToolCalls: sess.Counters.ToolCalls,
	})
}

// prompt handles POST /v1/sessions/{id}/prompt, streaming the run's events as
// Server-Sent Events. It starts a run on the shared engine and relays each
// session.Event (mapped to the proto Event, JSON-encoded) as one SSE frame.
func (h *HTTPHandler) prompt(w http.ResponseWriter, r *http.Request) {
	id := session.SessionID(r.PathValue("id"))
	var body promptBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	run, err := h.svc.StartRun(r.Context(), id, body.Text)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer h.svc.deregister(id, run)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// If the client disconnects, cancel the run.
	go func() {
		<-r.Context().Done()
		run.Cancel()
	}()

	enc := json.NewEncoder(w)
	for ev := range run.Events() {
		if _, err := w.Write([]byte("data: ")); err != nil {
			run.Cancel()
			return
		}
		if err := enc.Encode(toProto(ev)); err != nil { // Encode appends a newline
			run.Cancel()
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			run.Cancel()
			return
		}
		flusher.Flush()
	}
}

// approve handles POST /v1/sessions/{id}/approve, resolving the paused ask on
// the session's in-flight run.
func (h *HTTPHandler) approve(w http.ResponseWriter, r *http.Request) {
	id := session.SessionID(r.PathValue("id"))
	var body approveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.AskID == "" {
		writeError(w, http.StatusBadRequest, "ask_id is required")
		return
	}
	run, ok := h.svc.LookupRun(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no in-flight run for session")
		return
	}
	run.Approve(body.AskID, body.Allow)
	w.WriteHeader(http.StatusNoContent)
}

// cancel handles POST /v1/sessions/{id}/cancel, cancelling the in-flight run.
func (h *HTTPHandler) cancel(w http.ResponseWriter, r *http.Request) {
	id := session.SessionID(r.PathValue("id"))
	run, ok := h.svc.LookupRun(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no in-flight run for session")
		return
	}
	run.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------------

// modeFromString maps a JSON mode string to a session.PermissionMode. Unknown
// or empty values fall through to the empty mode (Service applies its default).
func modeFromString(s string) session.PermissionMode {
	switch strings.ToLower(s) {
	case "plan":
		return session.ModePlan
	case "acceptedits", "accept_edits", "accept":
		return session.ModeAccept
	case "default":
		return session.ModeDefault
	default:
		return ""
	}
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON {"error": msg} body with the given status code.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeServiceError maps a service sentinel error to an HTTP status.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
