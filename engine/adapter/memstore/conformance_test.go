package memstore_test

import (
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/storeconformance"
	"github.com/stacklok/mecatl/engine/port"
)

// TestMemstoreConformance runs the shared SessionStore conformance table
// against the in-memory reference store. This run IS the suite's in-engine
// validation (there is deliberately no separate self-test fake).
func TestMemstoreConformance(t *testing.T) {
	storeconformance.Run(t, func(*testing.T) port.SessionStore {
		return memstore.New()
	})
}

// TestMemstorePrunableConformance runs the shared PrunableStore (retention
// seam) table against the in-memory reference store.
func TestMemstorePrunableConformance(t *testing.T) {
	storeconformance.RunPrunable(t, func(*testing.T) port.SessionStore {
		return memstore.New()
	})
}
