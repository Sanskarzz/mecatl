package app

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/grpcdriver"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
)

// TestValidateDriverConfigExclusivity pins the mutual-exclusion rule: a local
// dir and a remote driver URL for the SAME store is a fatal config error;
// every other combination passes.
func TestValidateDriverConfigExclusivity(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "all empty", cfg: Config{}},
		{name: "local only", cfg: Config{StoreDir: "/tmp/s", MemoryDir: "/tmp/m"}},
		{name: "drivers only", cfg: Config{SessionStoreURL: "127.0.0.1:7443", MemoryStoreURL: "127.0.0.1:7443"}},
		{name: "mixed across seams", cfg: Config{StoreDir: "/tmp/s", MemoryStoreURL: "127.0.0.1:7443"}},
		{
			name:    "session store both",
			cfg:     Config{StoreDir: "/tmp/s", SessionStoreURL: "127.0.0.1:7443"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "memory store both",
			cfg:     Config{MemoryDir: "/tmp/m", MemoryStoreURL: "127.0.0.1:7443"},
			wantErr: "mutually exclusive",
		},
		{name: "tls with files", cfg: Config{SessionStoreURL: "10.0.0.9:7443", DriverTLS: true, DriverTLSCA: "/tmp/ca.pem", DriverTLSCert: "/tmp/c.pem", DriverTLSKey: "/tmp/k.pem"}},
		{
			name:    "tls ca without driver-tls",
			cfg:     Config{SessionStoreURL: "127.0.0.1:7443", DriverTLSCA: "/tmp/ca.pem"},
			wantErr: "require --driver-tls",
		},
		{
			name:    "tls cert without driver-tls",
			cfg:     Config{SessionStoreURL: "127.0.0.1:7443", DriverTLSCert: "/tmp/c.pem"},
			wantErr: "require --driver-tls",
		},
		{
			name:    "tls key without driver-tls",
			cfg:     Config{SessionStoreURL: "127.0.0.1:7443", DriverTLSKey: "/tmp/k.pem"},
			wantErr: "require --driver-tls",
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

// TestBuildStoreDriverURL pins the new buildStore branch: a SessionStoreURL
// yields the grpcdriver client. Offline-safe — grpc.NewClient is lazy, so no
// connection is attempted; the close func releases the (never-connected)
// conn.
func TestBuildStoreDriverURL(t *testing.T) {
	st, closeFn, err := buildStore(Config{SessionStoreURL: "127.0.0.1:7443"})
	if err != nil {
		t.Fatalf("buildStore(driver URL): %v", err)
	}
	defer closeFn()
	if _, ok := st.(*grpcdriver.SessionStore); !ok {
		t.Fatalf("buildStore(driver URL) = %T, want *grpcdriver.SessionStore", st)
	}
}

// TestBuildStoreDefaults pins that the existing branches are untouched: empty
// config still yields the in-memory store, StoreDir still yields the JSONL
// store.
func TestBuildStoreDefaults(t *testing.T) {
	t.Run("empty -> memstore", func(t *testing.T) {
		st, closeFn, err := buildStore(Config{})
		if err != nil {
			t.Fatalf("buildStore(empty): %v", err)
		}
		defer closeFn()
		if _, ok := st.(*memstore.Store); !ok {
			t.Fatalf("buildStore(empty) = %T, want *memstore.Store", st)
		}
	})
	t.Run("store-dir -> jsonlstore", func(t *testing.T) {
		st, closeFn, err := buildStore(Config{StoreDir: t.TempDir()})
		if err != nil {
			t.Fatalf("buildStore(StoreDir): %v", err)
		}
		defer closeFn()
		if _, ok := st.(*jsonlstore.Store); !ok {
			t.Fatalf("buildStore(StoreDir) = %T, want *jsonlstore.Store", st)
		}
	})
}

// TestDriverConnsShareEqualTargets pins the connection cache: two dials of
// the SAME target share one ClientConn (a deployment pointing both stores at
// one driver multiplexes one connection), distinct targets do not, and the
// once-guarded close tolerates being run from both consumers' chains.
func TestDriverConnsShareEqualTargets(t *testing.T) {
	conns := newDriverConns()
	cfg := Config{}
	c1, close1, err := conns.dial(cfg, "127.0.0.1:7443")
	if err != nil {
		t.Fatalf("dial #1: %v", err)
	}
	c2, close2, err := conns.dial(cfg, "127.0.0.1:7443")
	if err != nil {
		t.Fatalf("dial #2: %v", err)
	}
	if c1 != c2 {
		t.Error("equal targets returned distinct ClientConns, want one shared conn")
	}
	c3, close3, err := conns.dial(cfg, "127.0.0.1:7444")
	if err != nil {
		t.Fatalf("dial #3: %v", err)
	}
	if c3 == c1 {
		t.Error("distinct targets share one ClientConn, want separate conns")
	}
	close1()
	close2() // second close of the shared conn must be a no-op, not a panic/double-close
	close3()
}

// TestBuildCatalogMemoryDriverRegistersTools proves the REAL wiring
// (buildCatalog, the same function buildEngine calls) takes the
// MemoryStoreURL driver branch and registers the memory tool family — the
// driver-backed analogue of TestBuildCatalogRegistersMemorySearchWhenEnabled.
// Offline: grpc.NewClient is lazy, so registration needs no live driver.
func TestBuildCatalogMemoryDriverRegistersTools(t *testing.T) {
	ctx := context.Background()
	provider := mockllm.New(mockllm.TextTurn("x"))
	hooks := hookexec.New(nil)

	cfg := Config{MemoryStoreURL: "127.0.0.1:7443"}
	cat, assets, _, _, closeFn, err := buildCatalog(ctx, cfg, regForTest(provider, providerMock, cfg.Model), provider, hooks, agents.NewRegistry(nil), memstore.New())
	if err != nil {
		t.Fatalf("buildCatalog(memory driver): %v", err)
	}
	defer closeFn()

	for _, name := range []string{memory.SearchMemoryToolName, memory.RecallToolName, memory.RememberToolName} {
		if _, ok := cat.Lookup(name); !ok {
			t.Errorf("memory driver enabled (MemoryStoreURL set): catalog is missing %q", name)
		}
	}
	if _, ok := assets.memStore.(*grpcdriver.MemoryStore); !ok {
		t.Errorf("assets.memStore = %T, want *grpcdriver.MemoryStore (the driver branch must have been taken)", assets.memStore)
	}
}

// TestBuildRejectsExclusiveStoreConfig pins the Build()-level call site of
// validateDriverConfig: an exclusivity misconfig fails the WHOLE build, not
// just the helper in isolation.
func TestBuildRejectsExclusiveStoreConfig(t *testing.T) {
	_, err := Build(context.Background(), Config{StoreDir: t.TempDir(), SessionStoreURL: "127.0.0.1:7443"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Build(StoreDir+SessionStoreURL) error = %v, want the mutual-exclusion config error", err)
	}
}
