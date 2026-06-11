package jsonlstore_test

import (
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/storeconformance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
)

// TestJSONLStoreConformance runs the shared SessionStore conformance table
// against the append-only JSONL replay store (the osfs/fsconformance
// precedent for an internal adapter importing an engine/adapter test suite).
func TestJSONLStoreConformance(t *testing.T) {
	storeconformance.Run(t, func(t *testing.T) port.SessionStore {
		st, err := jsonlstore.New(t.TempDir())
		if err != nil {
			t.Fatalf("jsonlstore.New: %v", err)
		}
		return st
	})
}
