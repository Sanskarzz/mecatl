package server

import "github.com/stacklok/mecatl/engine/session"

// SessionEngineContextWindowForTest exposes the per-session engine's compaction
// context window (Engine.ContextWindow(), engine/agent/loop.go) for the session
// under id, or (0, false) if no per-session engine is registered (the session is
// riding the shared engine). It lets an EXTERNAL (server_test) test assert the
// ACTUAL compaction window of a rehydrated engine — a DIRECT check that does not
// rely on the ResolvedModel echo as a proxy (issue #66 engine-window fix review).
func (s *Service) SessionEngineContextWindowForTest(id session.SessionID) (int, bool) {
	s.mu.Lock()
	se, ok := s.sessionEngines[id]
	s.mu.Unlock()
	if !ok || se.engine == nil {
		return 0, false
	}
	return se.engine.ContextWindow(), true
}
