package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/sourceconformance"
	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/grpcdriver"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/skills"
)

// startSourceDriver serves the given driver services on a loopback listener
// (offline: 127.0.0.1 only) and returns the dial target. The server stops at
// test cleanup so the goleak gate stays clean.
func startSourceDriver(t *testing.T, register func(gs *grpc.Server)) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// soulDriverBody is a fixed-body prompt.SoulSource for the driver fixtures.
type soulDriverBody string

func (b soulDriverBody) Load(context.Context) (string, error) { return string(b), nil }

// driverConnsForTest returns a build-scoped conn cache whose every dialled
// conn closes at cleanup, so direct selectSoulSource/seam calls (which in
// production share Build's cache and its folded closes) stay goleak-clean.
func driverConnsForTest(t *testing.T) *driverConns {
	t.Helper()
	dc := newDriverConns()
	t.Cleanup(func() {
		dc.mu.Lock()
		defer dc.mu.Unlock()
		for _, c := range dc.conns {
			c.close()
		}
	})
	return dc
}

// TestValidateDriverConfigSourceExclusivity pins the new Phase-C1 rows: a
// local source and a driver URL for the same content seam are mutually
// exclusive; a lone URL passes.
func TestValidateDriverConfigSourceExclusivity(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "skill driver alone", cfg: Config{SkillSourceURL: "127.0.0.1:7443"}},
		{name: "soul driver alone", cfg: Config{SoulSourceURL: "127.0.0.1:7443"}},
		{name: "soul driver with no-soul", cfg: Config{SoulSourceURL: "127.0.0.1:7443", NoSoul: true}},
		{
			name:    "skill driver with explicit dirs",
			cfg:     Config{SkillSourceURL: "127.0.0.1:7443", SkillsDirs: []string{"/tmp/sk"}},
			wantErr: "mutually exclusive",
		},
		{
			name:    "skill driver with conventional",
			cfg:     Config{SkillSourceURL: "127.0.0.1:7443", SkillsConventional: true},
			wantErr: "mutually exclusive",
		},
		{
			name:    "soul driver with soul file",
			cfg:     Config{SoulSourceURL: "127.0.0.1:7443", SoulPath: "/tmp/soul.md"},
			wantErr: "mutually exclusive",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateDriverConfig(c.cfg)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDriverConfig = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("validateDriverConfig = %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestResolveDriverSkillSeam drives the REAL driver branch end to end over a
// loopback fixture server: the metadata snapshot, the single cache read root,
// lazy materialization on first activation (executable bit honored, base dir
// inside the cache), and cache removal on close.
func TestResolveDriverSkillSeam(t *testing.T) {
	addr := startSourceDriver(t, func(gs *grpc.Server) {
		driverv1.RegisterSkillSourceServiceServer(gs, grpcdriver.NewSkillSourceServer(sourceconformance.NewFixtureSource()))
	})
	cfg := Config{SkillSourceURL: addr}
	seam, err := resolveSkillSeam(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("resolveSkillSeam: %v", err)
	}
	if len(seam.metas) != len(sourceconformance.Fixture) {
		t.Fatalf("seam.metas = %d skills, want %d", len(seam.metas), len(sourceconformance.Fixture))
	}

	// readRoots == {cacheBase}: exactly one root, an existing dir (EAGER
	// creation — a late-born root would be unreadable by osfs).
	if len(seam.readRoots) != 1 {
		t.Fatalf("seam.readRoots = %v, want exactly the asset cache", seam.readRoots)
	}
	cacheBase := seam.readRoots[0]
	if fi, serr := os.Stat(cacheBase); serr != nil || !fi.IsDir() {
		t.Fatalf("asset cache %q must exist at build time: %v", cacheBase, serr)
	}

	// LAZY: nothing materialized before the first activation.
	if entries, _ := os.ReadDir(cacheBase); len(entries) != 0 {
		t.Errorf("asset cache must be empty before any activation, got %v", entries)
	}

	act, err := seam.activator.Activate(context.Background(), "review")
	if err != nil {
		t.Fatalf("Activate(review): %v", err)
	}
	if act.BaseDir != filepath.Join(cacheBase, "review") {
		t.Errorf("BaseDir = %q, want %q", act.BaseDir, filepath.Join(cacheBase, "review"))
	}
	script := filepath.Join(act.BaseDir, "scripts", "lint.sh")
	if fi, serr := os.Stat(script); serr != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("materialized script %q must exist with the executable bit: fi=%v err=%v", script, fi, serr)
	}

	// An asset-less driver skill omits the base dir.
	lean, err := seam.activator.Activate(context.Background(), "commit-style")
	if err != nil {
		t.Fatalf("Activate(commit-style): %v", err)
	}
	if lean.BaseDir != "" {
		t.Errorf("asset-less skill BaseDir = %q, want \"\"", lean.BaseDir)
	}

	// Close removes the cache.
	seam.close()
	if _, serr := os.Stat(cacheBase); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("seam close must remove the asset cache, stat err = %v", serr)
	}
}

// TestBuildCatalogSkillDriverRegistersSkillTool proves the REAL wiring
// (buildCatalog) takes the SkillSourceURL branch: the Skill tool registers
// over the driver metas, the assets carry the cache read root, and the
// catalog close removes the cache — the driver-backed analogue of
// TestBuildCatalogMemoryDriverRegistersTools.
func TestBuildCatalogSkillDriverRegistersSkillTool(t *testing.T) {
	addr := startSourceDriver(t, func(gs *grpc.Server) {
		driverv1.RegisterSkillSourceServiceServer(gs, grpcdriver.NewSkillSourceServer(sourceconformance.NewFixtureSource()))
	})
	ctx := context.Background()
	provider := mockllm.New(mockllm.TextTurn("x"))
	cfg := Config{SkillSourceURL: addr}

	cat, assets, _, _, closeFn, err := buildCatalog(ctx, cfg, regForTest(provider, providerMock, cfg.Model), provider, hookexec.New(nil), agents.NewRegistry(nil), memstore.New())
	if err != nil {
		t.Fatalf("buildCatalog(skill driver): %v", err)
	}
	if _, ok := cat.Lookup(skills.ToolName); !ok {
		t.Error("skill driver enabled (SkillSourceURL set): catalog is missing the Skill tool")
	}
	if len(assets.skills) != len(sourceconformance.Fixture) {
		t.Errorf("assets.skills = %d metas, want %d", len(assets.skills), len(sourceconformance.Fixture))
	}
	if len(assets.skillReadRoots) != 1 {
		t.Fatalf("assets.skillReadRoots = %v, want exactly the asset cache", assets.skillReadRoots)
	}
	cacheBase := assets.skillReadRoots[0]
	if _, serr := os.Stat(cacheBase); serr != nil {
		t.Fatalf("asset cache must exist: %v", serr)
	}
	closeFn()
	if _, serr := os.Stat(cacheBase); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("catalog close must remove the asset cache, stat err = %v", serr)
	}
}

// TestResolveDriverSkillSeamUnreachableFatal pins the loud-misconfig posture:
// an explicitly configured skill driver that cannot answer the build-time
// ListSkills snapshot fails the seam (and therefore Build), never a silent
// no-skills degradation.
func TestResolveDriverSkillSeamUnreachableFatal(t *testing.T) {
	// A listener that is immediately closed: the port refuses connections.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	if _, err := resolveSkillSeam(context.Background(), Config{SkillSourceURL: addr}, nil); err == nil {
		t.Fatal("resolveSkillSeam(unreachable driver) = nil error, want a fatal snapshot failure")
	}
}

// TestSelectSoulSourceDriverPrecedence pins §J: the driver occupies the USER
// slot (it SHADOWS a present, trusted project soul), carries soulDriver
// provenance with Trusted=true, and still respects --no-soul and the
// soul:apply gate.
func TestSelectSoulSourceDriverPrecedence(t *testing.T) {
	const persona = "Calm, precise, driver-served."
	addr := startSourceDriver(t, func(gs *grpc.Server) {
		driverv1.RegisterSoulSourceServiceServer(gs, grpcdriver.NewSoulSourceServer(soulDriverBody(persona)))
	})

	// A workspace WITH a trusted project soul that must be shadowed.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".mecatl"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".mecatl", "soul.md"), []byte("PROJECT persona"), 0o644); err != nil {
		t.Fatalf("write project soul: %v", err)
	}

	cfg := Config{SoulSourceURL: addr, Workspace: ws, TrustProject: true, driverConns: driverConnsForTest(t)}
	src, meta := selectSoulSource(cfg, newFakeIO().io(), fakeGate(governance.Allow))
	if src == nil || !meta.Present {
		t.Fatalf("driver soul must be selected, got src=%v meta=%+v", src, meta)
	}
	if meta.Provenance != soulDriver || !meta.Trusted {
		t.Errorf("meta = %+v, want Provenance=driver Trusted=true", meta)
	}
	if meta.Drifted {
		t.Error("the drift baseline is SKIPPED for driver provenance; Drifted must be false")
	}
	body, err := src.Load(context.Background())
	if err != nil || body != persona {
		t.Errorf("Load = %q, %v; want the driver persona (project soul shadowed)", body, err)
	}

	// --no-soul wins over the driver.
	if src, meta := selectSoulSource(Config{SoulSourceURL: addr, NoSoul: true}, newFakeIO().io(), fakeGate(governance.Allow)); src != nil || meta.Present {
		t.Errorf("--no-soul must win over the driver, got src=%v meta=%+v", src, meta)
	}

	// The soul:apply gate runs UNCHANGED before the driver branch.
	if src, meta := selectSoulSource(Config{SoulSourceURL: addr}, newFakeIO().io(), fakeGate(governance.Deny)); src != nil || meta.Present {
		t.Errorf("soul:apply Deny must withhold the driver soul, got src=%v meta=%+v", src, meta)
	}

	// The snapshot projection carries the new provenance.
	if got := soulProvenanceProto(soulDriver); got.String() != "SOUL_PROVENANCE_DRIVER" {
		t.Errorf("soulProvenanceProto(soulDriver) = %v, want SOUL_PROVENANCE_DRIVER", got)
	}
}

// TestBuildSoulDriverProbeFatal pins the build-time posture: an explicitly
// configured soul driver that cannot answer the probe fails the WHOLE build.
func TestBuildSoulDriverProbeFatal(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	_, err = Build(context.Background(), Config{
		Workspace:     t.TempDir(),
		Model:         "mock",
		UseMock:       true,
		SoulSourceURL: addr,
	})
	if err == nil || !strings.Contains(err.Error(), "soul-source driver") {
		t.Fatalf("Build(unreachable soul driver) error = %v, want the fatal probe error", err)
	}
}

// countingSkillSource wraps a tool.SkillSource and counts SkillBody fetches,
// so the driver preload index's LAZY contract is observable.
type countingSkillSource struct {
	tool.SkillSource
	bodyCalls map[string]int
}

func (c *countingSkillSource) SkillBody(ctx context.Context, name string) (string, error) {
	c.bodyCalls[name]++
	return c.SkillSource.SkillBody(ctx, name)
}

// TestDriverSkillIndexLazy pins the driver preload index's laziness: only the
// skill names an agent definition actually references are fetched — each
// EXACTLY ONCE (de-duped across repeated references), and an unreferenced
// skill transfers no body at all.
func TestDriverSkillIndexLazy(t *testing.T) {
	src := &countingSkillSource{SkillSource: sourceconformance.NewFixtureSource(), bodyCalls: map[string]int{}}
	metas, err := src.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	reg := agents.NewRegistry([]agents.AgentDef{
		{Name: "spec-a", Skills: []string{"review", "ghost"}},
		{Name: "spec-b", Skills: []string{"review"}}, // re-reference: must not re-fetch
	})

	idx := driverSkillIndex(context.Background(), Config{}, src, metas, reg)
	if got := src.bodyCalls["review"]; got != 1 {
		t.Errorf("referenced skill body fetched %d times, want exactly 1", got)
	}
	for _, never := range []string{"commit-style", "research", "ghost"} {
		if got := src.bodyCalls[never]; got != 0 {
			t.Errorf("skill %q body fetched %d times, want 0 (lazy: %s)", never, got,
				"only def-referenced, source-known names transfer")
		}
	}
	if body, ok := idx["review"]; !ok || body == "" {
		t.Errorf("index missing the referenced body: %+v", idx)
	}
	if _, ok := idx["commit-style"]; ok {
		t.Errorf("index carries an unreferenced body: %+v", idx)
	}
}

// TestDriverAssetReadableThroughEarlyWorkspace pins the seam join the EAGER
// cache MkdirTemp exists for: a production osfs Workspace is constructed
// BEFORE any activation (the cache root is registered while still empty),
// a skill then materializes its payloads LATE, and a Read of the materialized
// file by the absolute path the activation header advertises succeeds through
// that pre-existing workspace.
func TestDriverAssetReadableThroughEarlyWorkspace(t *testing.T) {
	addr := startSourceDriver(t, func(gs *grpc.Server) {
		driverv1.RegisterSkillSourceServiceServer(gs, grpcdriver.NewSkillSourceServer(sourceconformance.NewFixtureSource()))
	})
	cfg := Config{SkillSourceURL: addr}
	seam, err := resolveSkillSeam(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("resolveSkillSeam: %v", err)
	}
	t.Cleanup(seam.close)

	// Workspace FIRST: the factory opens its read roots at construction; the
	// cache root exists (eager MkdirTemp) but holds nothing yet.
	ws := osfsWorkspaceFactory(Config{}.diag(), seam.readRoots)(t.TempDir())
	if ws == nil {
		t.Fatal("workspace factory returned nil")
	}

	// Activation SECOND: the bundle materializes after the workspace was built.
	act, err := seam.activator.Activate(context.Background(), "review")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if act.BaseDir == "" {
		t.Fatal("review must materialize a base dir")
	}

	absFile := filepath.Join(act.BaseDir, "references", "checklist.md")
	got, err := ws.Read(context.Background(), absFile)
	if err != nil {
		t.Fatalf("workspace Read(%q) after late materialization: %v (the eager cache-root registration is broken)", absFile, err)
	}
	if want := "- correctness first\n- style second\n"; string(got) != want {
		t.Errorf("Read = %q, want %q", got, want)
	}
}
