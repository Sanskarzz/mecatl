// childgc.go is the retention/GC POLICY for persisted child sessions (issue
// #38). The delegation paths persist every child snapshot best-effort
// (WithSubagentStore / WithMemberStore / the Parallel branches) so
// InspectSubagent / InspectMember / resume: work — but nothing ever deleted
// them, so a durable store grew without bound. The MECHANISM (List/Delete)
// is the optional port.PrunableStore seam each store adapter implements; the
// POLICY (what is a child, how old is too old, how many per family, who is
// live) lives HERE, in composition — the stores never learn it.

package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

// childSessionPrefixes are the delegation families' child-session id
// prefixes, consumed DIRECTLY from the engine's exported id-minting
// convention (engine/agent/childregistry.go — the constants the minting
// sites themselves derive from, so this list cannot drift from what is
// actually minted; TestChildSessionPrefixesMatchEngineConvention pins the
// wiring). ANY id outside these prefixes — every operator/service session —
// is NEVER touched by the sweeper.
//
// SCOPE CAVEAT: a deployment that overrides a family's prefix
// (agent.WithChildSessionPrefix / WithParallelChildSessionPrefix /
// WithMemberSessionPrefix) mints ids OUTSIDE this list and thereby DE-SCOPES
// them from GC entirely — those children accumulate unswept until a matching
// policy knob exists. mecatl's own composition never overrides the prefixes.
var childSessionPrefixes = []string{
	agent.SubagentSessionPrefix,
	agent.ParallelSessionPrefix,
	agent.TeamSessionPrefix,
}

// isChildSession reports whether id carries one of the delegation families'
// prefixes, returning the matched prefix (the per-family cap's grouping key).
func isChildSession(id session.SessionID) (family string, ok bool) {
	for _, p := range childSessionPrefixes {
		if strings.HasPrefix(string(id), p) {
			return p, true
		}
	}
	return "", false
}

// childGCPolicy is the operator-tunable retention policy.
type childGCPolicy struct {
	// retention is the age threshold for the age pass: a child snapshot whose
	// ModifiedAt is older than now-retention is deleted. <=0 disables the age
	// pass.
	retention time.Duration
	// maxPerFamily is the per-family count cap: within one prefix family the
	// newest maxPerFamily child snapshots survive, the rest are deleted
	// oldest-first. <=0 disables the cap pass.
	maxPerFamily int
}

// enabled reports whether any pass is active (the all-zero policy is the
// fully-disabled posture).
func (p childGCPolicy) enabled() bool { return p.retention > 0 || p.maxPerFamily > 0 }

// childGC sweeps child-session snapshots out of a prunable store per the
// policy. Everything is injected (store, clock, liveness, diagnostics) so the
// sweep is deterministic under test.
type childGC struct {
	store  port.PrunableStore
	policy childGCPolicy
	// isLive reports whether a session id has an in-flight run in this
	// process (Service.IsLive). A live id is never deleted, by EITHER pass.
	//
	// INVARIANT — the live-skip does NOT protect engine-spawned children.
	// Service.IsLive reads the TOP-LEVEL run registry only; a mid-run
	// subagent/parallel/team child is driven inside its parent's run and is
	// never registered there, so IsLive("subagent-…") answers false even
	// while that child is executing (pinned by
	// TestServiceIsLiveDoesNotKnowEngineChildren in internal/adapter/server).
	// Engine children are instead protected by AGE HORIZON + SNAPSHOT
	// FRESHNESS: every child persists at its terminal, and a RESUMED child
	// re-persists at resume start (engine/agent/subagent.go), so an
	// in-flight child's snapshot is always younger than any sane retention.
	// The seam stays because it IS protective for API-client-driven sessions
	// that carry a child prefix (StartRun on such an id registers it here).
	isLive func(session.SessionID) bool
	now    func() time.Time
	diag   port.Diagnostics
	// disabled is the sticky kill switch: set when the store signals
	// port.ErrPruneUnsupported (the backend can NEVER enumerate/delete), after
	// which every subsequent sweep is a no-op and the ticker goroutine exits.
	// Only the sweep goroutine (or a single-goroutine test) touches it.
	disabled bool
}

// sweep runs one age pass then one per-family cap pass and returns the
// (deleted, retained) child counts. It is BEST-EFFORT throughout: a List
// failure aborts the sweep with one WARN; Delete failures are tallied into
// one WARN per sweep (the entry stays retained and the next sweep retries);
// nothing is ever fatal. It logs ONE INFO summary when anything was deleted
// and stays silent otherwise (a found-nothing sweep is the steady state).
// A store that signals port.ErrPruneUnsupported (the backend can NEVER
// enumerate — e.g. a remote driver answering UNIMPLEMENTED) gets ONE INFO and
// stickily disables all further sweeping — never a recurring WARN.
func (g *childGC) sweep(ctx context.Context) (deleted, retained int) {
	if g.disabled {
		return 0, 0
	}
	entries, err := g.store.List(ctx)
	if err != nil {
		if errors.Is(err, port.ErrPruneUnsupported) {
			g.disabled = true
			g.diag.Log(ctx, port.LevelInfo, "child-session GC: store does not support retention; disabling child GC", "err", err)
			return 0, 0
		}
		g.diag.Log(ctx, port.LevelWarn, "child-session GC: list failed; skipping sweep", "err", err)
		return 0, 0
	}

	// Partition the inventory into the per-family child sets. Unprefixed ids
	// (operator/service sessions) are NOT collected: they are invisible to
	// both passes by construction.
	byFamily := make(map[string][]port.StoredSession, len(childSessionPrefixes))
	for _, e := range entries {
		family, ok := isChildSession(e.ID)
		if !ok {
			continue
		}
		byFamily[family] = append(byFamily[family], e)
	}

	var errs deleteErrors
	for _, kids := range byFamily {
		deleted += g.sweepFamily(ctx, kids, &errs)
	}

	for _, kids := range byFamily {
		retained += len(kids)
	}
	retained -= deleted

	if errs.count > 0 {
		g.diag.Log(ctx, port.LevelWarn, "child-session GC: some deletes failed (retained; retried next sweep)",
			"failed", errs.count, "first_err", errs.first)
	}
	if deleted > 0 {
		g.diag.Log(ctx, port.LevelInfo, "child-session GC: swept",
			"deleted", deleted, "retained", retained)
	}
	return deleted, retained
}

// deleteErrors tallies a sweep's failed deletes into ONE WARN (first error
// kept as the sample).
type deleteErrors struct {
	count int
	first error
}

// remove best-effort-deletes one id, tallying a failure into errs.
func (g *childGC) remove(ctx context.Context, id session.SessionID, errs *deleteErrors) bool {
	if err := g.store.Delete(ctx, id); err != nil {
		errs.count++
		if errs.first == nil {
			errs.first = err
		}
		return false
	}
	return true
}

// sweepFamily runs the age pass then the count-cap pass over ONE family's
// entries and returns how many it deleted.
func (g *childGC) sweepFamily(ctx context.Context, kids []port.StoredSession, errs *deleteErrors) (deleted int) {
	// Oldest-first within the family: the age pass deletes a prefix of this
	// order and the cap pass keeps a suffix of it. Equal ModifiedAt (coarse
	// file mtimes, batch saves) tie-breaks on ID so the cap pass evicts
	// DETERMINISTICALLY — store List order is unspecified (map iteration),
	// and without the tiebreak which sibling survives would be random.
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].ModifiedAt.Equal(kids[j].ModifiedAt) {
			return kids[i].ID < kids[j].ID
		}
		return kids[i].ModifiedAt.Before(kids[j].ModifiedAt)
	})

	survivors := kids
	if g.policy.retention > 0 {
		survivors = kids[:0]
		cutoff := g.now().Add(-g.policy.retention)
		for _, e := range kids {
			if e.ModifiedAt.Before(cutoff) && !g.isLive(e.ID) && g.remove(ctx, e.ID, errs) {
				deleted++
				continue
			}
			survivors = append(survivors, e)
		}
	}

	if g.policy.maxPerFamily <= 0 || len(survivors) <= g.policy.maxPerFamily {
		return deleted
	}
	over := len(survivors) - g.policy.maxPerFamily
	for _, e := range survivors {
		if over == 0 {
			break
		}
		if g.isLive(e.ID) {
			// A live id keeps its slot; the next-oldest NON-live id is
			// deleted in its stead (the loop keeps scanning).
			continue
		}
		// Best-effort: a failed delete still consumes the attempt — the
		// family stays above cap until the next sweep retries it.
		if g.remove(ctx, e.ID, errs) {
			deleted++
		}
		over--
	}
	return deleted
}

// startChildGC wires the child-session retention sweeper: a no-op (with one
// build-once INFO, the startMemoryConsolidation idiom) when the policy is
// fully disabled or the store is not prunable; otherwise one startup sweep
// plus a ticker every cfg.ChildGCInterval (0 = startup-only), all on one
// background goroutine sharing ctx (so the loop exits on shutdown). The
// liveness predicate is the Service's in-flight run registry, threaded in by
// Build AFTER the Service exists.
func startChildGC(ctx context.Context, cfg Config, store port.SessionStore, isLive func(session.SessionID) bool) {
	policy := childGCPolicy{retention: cfg.ChildRetention, maxPerFamily: cfg.ChildRetentionMaxPerFamily}
	if !policy.enabled() {
		cfg.diag().Log(ctx, port.LevelInfo, "child-session GC DISABLED (no retention and no per-family cap)")
		return
	}
	prunable, ok := store.(port.PrunableStore)
	if !ok {
		cfg.diag().Log(ctx, port.LevelInfo, "child-session GC unavailable (session store is not prunable); store is never swept")
		return
	}
	gc := &childGC{
		store:  prunable,
		policy: policy,
		isLive: isLive,
		now:    time.Now,
		diag:   cfg.diag(),
	}
	cfg.diag().Log(ctx, port.LevelInfo, "child-session GC ENABLED",
		"retention", cfg.ChildRetention, "max_per_family", cfg.ChildRetentionMaxPerFamily,
		"interval", cfg.ChildGCInterval)
	go func() {
		gc.sweep(ctx)
		if cfg.ChildGCInterval <= 0 || gc.disabled {
			return // startup-only, or the store can never be swept
		}
		ticker := time.NewTicker(cfg.ChildGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gc.sweep(ctx)
				if gc.disabled {
					return // sticky: the store signalled ErrPruneUnsupported
				}
			}
		}
	}()
}
