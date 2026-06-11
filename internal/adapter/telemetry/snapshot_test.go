package telemetry

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSnapshotJSONUsesCumulativeAllocKey pins the deliberate wire-key rename:
// the cumulative heap-allocation counter serializes as heap_allocs_total_bytes,
// and the old key heap_alloc_bytes (which read as the LIVE-gauge meaning of Go
// MemStats.HeapAlloc) must never reappear. It also asserts the runtime metric
// actually MAPS into the renamed field (mirroring the TotalMemoryBytes
// precedent in TestSnapshotPositiveGoroutinesAndRoundTrips): any Go test
// process has allocated, so a zero here means the /gc/heap/allocs switch arm
// was dropped, not that the value is legitimately zero.
func TestSnapshotJSONUsesCumulativeAllocKey(t *testing.T) {
	snap := Snapshot()
	if snap.HeapAllocsTotalBytes == 0 {
		t.Error("HeapAllocsTotalBytes = 0, expected the /gc/heap/allocs sample to populate it")
	}
	if !slices.Contains(snap.Available, metricHeapAllocsBytes) {
		t.Errorf("Available = %v, want it to contain %q", snap.Available, metricHeapAllocsBytes)
	}

	b, err := MarshalSnapshotJSON(snap)
	if err != nil {
		t.Fatalf("MarshalSnapshotJSON: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("unmarshal snapshot JSON: %v", err)
	}
	if _, ok := keys["heap_allocs_total_bytes"]; !ok {
		t.Errorf("snapshot JSON missing key %q; keys: %s", "heap_allocs_total_bytes", b)
	}
	if _, ok := keys["heap_alloc_bytes"]; ok {
		t.Errorf("snapshot JSON still carries the renamed key %q (collides with the live-gauge MemStats.HeapAlloc meaning)", "heap_alloc_bytes")
	}
}
