package agent

import (
	"container/list"
	"sync"
)

// PreservedForkStore is the seam the Fork tool uses to retain a winning branch's
// PRESERVED fork (join=first / join=judge) under a BOUNDED policy. A winner's
// fork is intentionally not torn down at the end of the call — its contents are
// the deliverable, inspectable/mergeable by the operator — but without a bound
// every winner over the process lifetime would leak a fork dir, growing disk
// without limit. A PreservedForkStore caps how many preserved forks survive at
// once: when a new winner pushes the count past the cap, the OLDEST preserved
// fork is reaped (its captured cleanup invoked).
//
// LAYERING: this lives in internal/agent (application layer) so ForkTool can be
// injected with it without importing an adapter. The cleanup func is the SAME
// teardown the forker handed runBranch — the reaper simply defers calling it
// (for the winner) until eviction, instead of the caller dropping it on the floor.
//
// Implementations must be safe for concurrent use: several Fork calls can finish
// concurrently and each preserves at most one winner.
type PreservedForkStore interface {
	// Preserve records a winning fork's root path and the cleanup that tears it
	// down. The store retains it (keeping the fork on disk) until the cap forces
	// its eviction, at which point it invokes cleanup. A nil cleanup is ignored
	// (nothing to reap); root is used only for identity/diagnostics.
	Preserve(root string, cleanup func() error)
}

// LRUForkReaper is a process-scoped, bounded PreservedForkStore: it keeps the
// most-recent cap preserved winner forks and reaps the OLDEST when a new winner
// exceeds the cap (invoking that fork's captured cleanup). This bounds the disk a
// run can accumulate from preserved winners while keeping the most recent winners
// inspectable. It is safe for concurrent use.
//
// A non-positive cap is normalised to DefaultPreservedForkCap. A reaper is NOT
// required for Fork to work — without one, ForkTool falls back to the original
// behaviour (winner forks are preserved forever); the reaper is the bound.
type LRUForkReaper struct {
	cap int

	mu    sync.Mutex
	order *list.List               // front = oldest, back = newest
	elems map[string]*list.Element // root -> element (dedupes re-preserved roots)
}

// preservedFork is one retained winner fork: its root and the cleanup that reaps
// it. Stored as a *list.Element value in the LRU order list.
type preservedFork struct {
	root    string
	cleanup func() error
}

// DefaultPreservedForkCap is the number of preserved winner forks an LRUForkReaper
// keeps when constructed with a non-positive cap. Small by design: preserved forks
// are full workspace copies, so the default trades a little inspectability headroom
// for a tight disk bound.
const DefaultPreservedForkCap = 8

// NewLRUForkReaper constructs a bounded reaper that keeps at most cap preserved
// winner forks (the most recent). A non-positive cap uses DefaultPreservedForkCap.
func NewLRUForkReaper(capacity int) *LRUForkReaper {
	if capacity <= 0 {
		capacity = DefaultPreservedForkCap
	}
	return &LRUForkReaper{
		cap:   capacity,
		order: list.New(),
		elems: make(map[string]*list.Element),
	}
}

// Preserve records a winner fork and reaps the oldest beyond the cap. Re-preserving
// the same root refreshes its recency (and adopts the new cleanup) rather than
// double-counting. A nil cleanup is ignored. Eviction cleanup runs OUTSIDE the lock
// so a slow filesystem teardown does not serialize concurrent Fork calls.
func (r *LRUForkReaper) Preserve(root string, cleanup func() error) {
	if cleanup == nil {
		return
	}

	var evicted []func() error
	r.mu.Lock()
	if el, ok := r.elems[root]; ok && root != "" {
		// Already tracked: refresh recency and adopt the latest cleanup.
		el.Value = preservedFork{root: root, cleanup: cleanup}
		r.order.MoveToBack(el)
	} else {
		el := r.order.PushBack(preservedFork{root: root, cleanup: cleanup})
		if root != "" {
			r.elems[root] = el
		}
	}
	for r.order.Len() > r.cap {
		front := r.order.Front()
		pf := front.Value.(preservedFork)
		r.order.Remove(front)
		if pf.root != "" {
			delete(r.elems, pf.root)
		}
		evicted = append(evicted, pf.cleanup)
	}
	r.mu.Unlock()

	for _, c := range evicted {
		if c != nil {
			_ = c()
		}
	}
}

// Len reports how many preserved forks the reaper currently retains. Test/diagnostic
// only.
func (r *LRUForkReaper) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}
