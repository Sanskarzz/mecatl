package grpcdriver

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/tool"
)

// MemoryStore is a tool.MemoryStore over a remote MemoryStoreService driver.
// It is PURE TRANSLATION: every behavioral guarantee (key validation,
// sorting, value omission on Index/Search, determinism) is the DRIVER's, and
// the memconformance suite run over this client is what pins it. Sanitization
// of model-written values stays harness-side in the memory tools.
type MemoryStore struct {
	client driverv1.MemoryStoreServiceClient
}

// compile-time assertion that MemoryStore satisfies the tool seam.
var _ tool.MemoryStore = (*MemoryStore)(nil)

// NewMemoryStore wraps an established driver connection (see Dial) as a
// tool.MemoryStore.
func NewMemoryStore(conn grpc.ClientConnInterface) *MemoryStore {
	return &MemoryStore{client: driverv1.NewMemoryStoreServiceClient(conn)}
}

// RememberEntry stores e on the driver, overwriting any existing entry under
// e.Key. The driver stamps UpdatedAt on write (the input value is advisory);
// a blank/whitespace-only key surfaces the driver's INVALID_ARGUMENT.
func (st *MemoryStore) RememberEntry(ctx context.Context, e tool.MemoryEntry) error {
	if _, err := st.client.RememberEntry(ctx, &driverv1.RememberEntryRequest{Entry: toProtoEntry(e)}); err != nil {
		return rpcErr(ctx, "remember entry", err)
	}
	return nil
}

// Recall returns the entry for the exact key. A driver miss (found=false) is
// (zero, false, nil) — never an error.
func (st *MemoryStore) Recall(ctx context.Context, key string) (tool.MemoryEntry, bool, error) {
	resp, err := st.client.Recall(ctx, &driverv1.RecallRequest{Key: key})
	if err != nil {
		return tool.MemoryEntry{}, false, rpcErr(ctx, "recall", err)
	}
	if !resp.GetFound() {
		return tool.MemoryEntry{}, false, nil
	}
	return fromProtoEntry(resp.GetEntry()), true, nil
}

// List returns all entries whose key has the given prefix (empty = all),
// key-sorted by the driver, values present.
func (st *MemoryStore) List(ctx context.Context, prefix string) ([]tool.MemoryEntry, error) {
	resp, err := st.client.List(ctx, &driverv1.ListRequest{Prefix: prefix})
	if err != nil {
		return nil, rpcErr(ctx, "list", err)
	}
	return fromProtoEntries(resp.GetEntries()), nil
}

// Forget deletes the entry for key on the driver; a missing key is not an
// error (idempotent).
func (st *MemoryStore) Forget(ctx context.Context, key string) error {
	if _, err := st.client.Forget(ctx, &driverv1.ForgetRequest{Key: key}); err != nil {
		return rpcErr(ctx, "forget", err)
	}
	return nil
}

// Index returns the driver's tier-0 routing table: key-sorted entries with
// values omitted and descriptions filled.
func (st *MemoryStore) Index(ctx context.Context) ([]tool.MemoryEntry, error) {
	resp, err := st.client.Index(ctx, &driverv1.IndexRequest{})
	if err != nil {
		return nil, rpcErr(ctx, "index", err)
	}
	return fromProtoEntries(resp.GetEntries()), nil
}

// Search returns up to k entries relevant to query, best-first per the
// driver's (deterministic) ranking, values omitted. k <= 0 selects the
// driver's default page size; a blank query yields an empty result.
func (st *MemoryStore) Search(ctx context.Context, query string, k int) ([]tool.MemoryEntry, error) {
	resp, err := st.client.Search(ctx, &driverv1.SearchRequest{Query: query, K: int32(k)}) //nolint:gosec // k is a small page size
	if err != nil {
		return nil, rpcErr(ctx, "search", err)
	}
	return fromProtoEntries(resp.GetEntries()), nil
}

// toProtoEntry projects a tool.MemoryEntry onto the wire form. A zero
// UpdatedAt stays nil (unset) rather than the epoch — the driver stamps the
// write time anyway (the field is advisory on input).
func toProtoEntry(e tool.MemoryEntry) *driverv1.MemoryEntry {
	pe := &driverv1.MemoryEntry{Key: e.Key, Value: e.Value, Description: e.Description}
	if !e.UpdatedAt.IsZero() {
		pe.UpdatedAt = timestamppb.New(e.UpdatedAt)
	}
	return pe
}

// fromProtoEntry projects a wire entry back onto tool.MemoryEntry. A nil/
// unset timestamp maps to the zero time.Time, never the Unix epoch.
func fromProtoEntry(pe *driverv1.MemoryEntry) tool.MemoryEntry {
	if pe == nil {
		return tool.MemoryEntry{}
	}
	var updated time.Time
	if ts := pe.GetUpdatedAt(); ts != nil {
		updated = ts.AsTime()
	}
	return tool.MemoryEntry{Key: pe.GetKey(), Value: pe.GetValue(), Description: pe.GetDescription(), UpdatedAt: updated}
}

func fromProtoEntries(pes []*driverv1.MemoryEntry) []tool.MemoryEntry {
	if pes == nil {
		return nil
	}
	out := make([]tool.MemoryEntry, len(pes))
	for i, pe := range pes {
		out[i] = fromProtoEntry(pe)
	}
	return out
}
