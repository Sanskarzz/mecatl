package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
	"github.com/stacklok/mecatl/internal/adapter/slogdiag"
)

// router_test.go covers the OPT-IN semantic Subagent model router (ADR 0031, Phase 5):
// the buildModelRouterTask closure (category→model mapping, precedence, fail-soft,
// breaker) and the end-to-end proof through the REAL composition that a routed
// delegation mints the child on the classifier-chosen model.

const (
	routerLarge = "claude-3-5-haiku-20241022" // catalogued (catAnthropicModel); the "large" category target
	routerSmall = "gpt-5-mini"                // the "small" category target
)

func routerTaxonomyCfg() Config {
	return Config{
		Model:               "session-model",
		SubagentModelRouter: true,
		RouterCategories: []permconfig.RouterCategory{
			{Name: "small", Description: "trivial mechanical tasks", Model: routerSmall},
			{Name: "large", Description: "deep reasoning, architecture", Model: routerLarge},
		},
		RouterDefaultCategory: "small",
	}
}

// OFF: with the flag unset (or no categories) buildModelRouterTask returns nil — the
// engine then carries no router and the run() hook's routeTask is nil (byte-identical).
func TestBuildModelRouterTaskOffWhenDisabled(t *testing.T) {
	prov := mockllm.New()
	reg := regForTest(prov, providerAnthropic, "session-model")

	cfg := routerTaxonomyCfg()
	cfg.SubagentModelRouter = false // flag off
	if fn := buildModelRouterTask(cfg, reg, prov, providerAnthropic, cfg.Model); fn != nil {
		t.Fatal("router task must be nil when the enable flag is off")
	}

	cfg = routerTaxonomyCfg()
	cfg.RouterCategories = nil // flag on but no taxonomy
	if fn := buildModelRouterTask(cfg, reg, prov, providerAnthropic, cfg.Model); fn != nil {
		t.Fatal("router task must be nil when there is no taxonomy (flag without categories cannot route)")
	}
}

// The closure classifies and maps the chosen category to its resolved concrete model.
func TestBuildModelRouterTaskMapsCategoryToModel(t *testing.T) {
	prov := mockllm.New(mockllm.TextTurn(`{"category":"large"}`))
	reg := regForTest(prov, providerAnthropic, "session-model")

	fn := buildModelRouterTask(routerTaxonomyCfg(), reg, prov, providerAnthropic, "session-model")
	if fn == nil {
		t.Fatal("router task must be non-nil when enabled with a taxonomy")
	}
	cat, model, ok := fn("redesign the storage layer")
	if !ok {
		t.Fatal("a valid classification must resolve")
	}
	if cat != "large" || model != routerLarge {
		t.Fatalf("routed (category, model) = (%q, %q), want (large, %q)", cat, model, routerLarge)
	}
}

// Alias interaction: a category whose Model selector is an ALIAS resolves through the
// operator-merged alias map (operator taxonomy targets are uncapped — the operator is
// authoritative).
func TestBuildModelRouterTaskResolvesCategoryAlias(t *testing.T) {
	prov := mockllm.New(mockllm.TextTurn(`{"category":"small"}`))
	reg := regForTest(prov, providerAnthropic, "session-model")

	cfg := routerTaxonomyCfg()
	cfg.RouterCategories[0].Model = "tiny"                            // category "small" → alias "tiny"
	cfg.ModelAliases = map[string]string{"tiny": "resolved-tiny-1.0"} // alias → concrete id

	fn := buildModelRouterTask(cfg, reg, prov, providerAnthropic, "session-model")
	_, model, ok := fn("rename a var")
	if !ok || model != "resolved-tiny-1.0" {
		t.Fatalf("aliased category routed to (%q, %v), want resolved-tiny-1.0 true", model, ok)
	}
}

// Fail-soft: a classifier MISS (garbage/unknown category) yields ok=false — the caller
// inherits the default model.
func TestBuildModelRouterTaskFailSoftOnMiss(t *testing.T) {
	prov := mockllm.New(mockllm.TextTurn("I am not sure, sorry."))
	reg := regForTest(prov, providerAnthropic, "session-model")

	fn := buildModelRouterTask(routerTaxonomyCfg(), reg, prov, providerAnthropic, "session-model")
	if _, _, ok := fn("x"); ok {
		t.Fatal("a classifier miss must be fail-soft (ok=false), never a fabricated route")
	}
}

// Fail-soft: a category mapping to an UNRESOLVABLE selector yields ok=false.
func TestBuildModelRouterTaskFailSoftOnUnresolvableTarget(t *testing.T) {
	prov := mockllm.New(mockllm.TextTurn(`{"category":"small"}`))
	reg := regForTest(prov, providerAnthropic, "session-model")

	cfg := routerTaxonomyCfg()
	cfg.RouterCategories[0].Model = "sonnet" // a built-in alias meaning inherit → unresolvable
	fn := buildModelRouterTask(cfg, reg, prov, providerAnthropic, "session-model")
	if _, _, ok := fn("x"); ok {
		t.Fatal("an unresolvable category target must be fail-soft (ok=false)")
	}
}

// foldOperatorModelRouter with no resolver (or no router block) is a no-op.
func TestFoldOperatorModelRouterNoOpWithoutResolver(t *testing.T) {
	cfg := Config{Model: "m", SubagentModelRouter: true}
	if got := foldOperatorModelRouter(cfg); got.RouterCategories != nil {
		t.Fatal("foldOperatorModelRouter with no resolver must be a no-op")
	}
	// A real resolver but NO models.router block is also a no-op.
	path := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(path, []byte("permissions:\n  allow: []\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	res := permconfig.New(permconfig.Options{ExplicitFiles: []string{path}})
	if got := foldOperatorModelRouter(Config{Model: "m", permResolver: res}); got.RouterCategories != nil {
		t.Fatal("a resolver with no models.router block must fold nothing")
	}
}

// foldOperatorModelRouter DROPS a malformed category (empty name/description/model)
// fail-soft with a WARN while keeping the valid one. A regression inverting/deleting the
// `name=="" || desc=="" || model==""` drop loop fails here.
func TestFoldOperatorModelRouterDropsMalformed(t *testing.T) {
	const yamlCfg = `
models:
  router:
    default-category: good
    classifier-slot: cheap
    categories:
      - name: good
        description: a valid category
        model: gpt-4o-mini
      - name: nodesc
        description: ""
        model: gpt-4o-mini
      - name: ""
        description: missing name
        model: gpt-4o-mini
      - name: nomodel
        description: missing model
        model: ""
`
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte(yamlCfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	res := permconfig.New(permconfig.Options{ExplicitFiles: []string{path}})
	if res == nil {
		t.Fatal("resolver should be non-nil")
	}
	diag := newCapturingDiagnostics()
	cfg := foldOperatorModelRouter(Config{Model: "m", Diagnostics: diag, permResolver: res})

	if len(cfg.RouterCategories) != 1 {
		t.Fatalf("want exactly 1 surviving category (the 3 malformed dropped), got %d: %+v", len(cfg.RouterCategories), cfg.RouterCategories)
	}
	if cfg.RouterCategories[0].Name != "good" || cfg.RouterCategories[0].Model != "gpt-4o-mini" {
		t.Fatalf("the valid category did not survive intact: %+v", cfg.RouterCategories[0])
	}
	if cfg.RouterDefaultCategory != "good" || cfg.RouterClassifierSlot != "cheap" {
		t.Fatalf("router header not folded: default=%q classifier-slot=%q", cfg.RouterDefaultCategory, cfg.RouterClassifierSlot)
	}
	if n := diag.countContaining("DROPPING a malformed category"); n != 3 {
		t.Fatalf("want 3 malformed-drop WARNs, got %d", n)
	}
}

// logModelRouterFacts: enabled + no taxonomy → a WARN (the real operator-misconfig
// signal); enabled + taxonomy → the ACTIVE INFO naming the category count + classifier.
func TestLogModelRouterFacts(t *testing.T) {
	t.Run("flag set, no taxonomy → WARN", func(t *testing.T) {
		diag := newCapturingDiagnostics()
		logModelRouterFacts(Config{Model: "m", SubagentModelRouter: true, Diagnostics: diag})
		if n := diag.countContaining("no models.router categories are configured"); n != 1 {
			t.Fatalf("want 1 no-taxonomy WARN, got %d", n)
		}
		if n := diag.countContaining("model router ACTIVE"); n != 0 {
			t.Fatalf("must NOT narrate ACTIVE without a taxonomy, got %d", n)
		}
	})
	t.Run("OFF → silent", func(t *testing.T) {
		diag := newCapturingDiagnostics()
		logModelRouterFacts(Config{Model: "m", SubagentModelRouter: false, Diagnostics: diag})
		if n := diag.countContaining("model router"); n != 0 {
			t.Fatalf("router OFF must be silent, got %d lines", n)
		}
	})
	t.Run("enabled + taxonomy → ACTIVE naming the classifier model", func(t *testing.T) {
		// Use a slogdiag buffer (not the message-only capturingDiagnostics) so the
		// structured `classifier` arg is rendered into the asserted line — the L1
		// alignment fix is that the LOGGED classifier matches what a session classifies on.
		var buf bytes.Buffer
		diag := slogdiag.New(&buf, false, port.LevelDebug)
		cfg := Config{
			Model:               "session-model",
			SubagentModelRouter: true,
			Diagnostics:         diag,
			ModelSlots:          map[string]string{slotRouter: "tiny"},
			ModelAliases:        map[string]string{"tiny": "classifier-id-1.0"},
			RouterCategories:    []permconfig.RouterCategory{{Name: "small", Description: "x", Model: "gpt-4o-mini"}},
		}
		logModelRouterFacts(cfg)
		log := buf.String()
		if !strings.Contains(log, "subagent model router ACTIVE") {
			t.Fatalf("want the ACTIVE INFO; got:\n%s", log)
		}
		if !strings.Contains(log, "classifier-id-1.0") {
			t.Fatalf("the ACTIVE INFO must name the resolved classifier model classifier-id-1.0; got:\n%s", log)
		}
		if !strings.Contains(log, "categories=1") {
			t.Fatalf("the ACTIVE INFO must name the category count; got:\n%s", log)
		}
	})
}

// The `router` slot (or classifier-slot) actually changes which model the CLASSIFIER
// engine is built on — its LLM request carries THAT model, not the session model.
func TestRouterClassifierRunsOnSlotModel(t *testing.T) {
	var (
		mu     sync.Mutex
		models []string
	)
	prov := observedProvider(&models, &mu, mockllm.TextTurn(`{"category":"large"}`))
	reg := regForTest(prov, providerAnthropic, "session-model")

	cfg := routerTaxonomyCfg()
	// Route the classifier onto a DISTINCT model via the `router` slot.
	cfg.ModelSlots = map[string]string{slotRouter: "classifier-only"}
	cfg.ModelAliases = map[string]string{"classifier-only": catAnthropicModel}

	fn := buildModelRouterTask(cfg, reg, prov, providerAnthropic, "session-model")
	if fn == nil {
		t.Fatal("router task must be non-nil")
	}
	cat, _, ok := fn("classify this")
	if !ok || cat != "large" {
		t.Fatalf("classification failed: cat=%q ok=%v", cat, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) == 0 || models[0] != catAnthropicModel {
		t.Fatalf("classifier LLM request model = %v, want the `router`-slot model %q (not the session model)", models, catAnthropicModel)
	}
}

// routerModelsObserved builds a provider whose request observer records every model and
// replays a parent→classifier→child→parent script. The classifier turn names "large".
func routerE2EProvider(models *[]string, mu *sync.Mutex) *mockllm.Provider {
	return mockllm.NewWith([]mockllm.Option{mockllm.WithRequestObserver(func(r port.LLMRequest) {
		mu.Lock()
		*models = append(*models, r.Model)
		mu.Unlock()
	})},
		mockllm.ToolCallTurn(session.NewToolCall("c1", "Subagent", []byte(`{"prompt":"redesign storage"}`))),
		mockllm.TextTurn(`{"category":"large"}`), // the classifier turn
		mockllm.TextTurn("CHILD SUMMARY"),        // the routed child's turn
		mockllm.TextTurn("parent done"),
	)
}

// END-TO-END: a plain Subagent delegation under an enabled router is classified "large"
// and the child runs on the large model; the parent's requests stay on the session
// model. The shared mock cursor serialises: parent → classifier → child → parent.
func TestRouterRoutesChildToClassifiedModelE2E(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	var (
		mu     sync.Mutex
		models []string
	)
	built, err := Build(ctx, Config{
		Workspace:           workspace,
		NoSoul:              true,
		Model:               "gpt-5",
		SubagentModelRouter: true,
		RouterCategories: []permconfig.RouterCategory{
			{Name: "small", Description: "trivial tasks", Model: routerSmall},
			{Name: "large", Description: "deep reasoning", Model: routerLarge},
		},
		RouterDefaultCategory: "small",
		AllowAllTools:         true,
		envDetector:           fakeEnv(map[string]string{"OPENAI_API_KEY": "sk-x"}),
		liveModelHTTPClient:   offlineHTTPClient(),
		providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
			return routerE2EProvider(&models, &mu)
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()

	sess, err := built.Service.CreateSession(ctx, workspace, session.ModeDefault, defaultLimits())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := built.Service.StartRun(ctx, sess.ID, "go")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got := drainRun(run); got != "parent done" {
		t.Fatalf("terminal text = %q, want parent done", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// The shared mock cursor serialises exactly: parent (Subagent call) → classifier →
	// routed child → parent (final). Assert the request POSITIONS, not just "some request
	// carried routerLarge" — so a future classifier-on-a-slot change (which would shift
	// the classifier off the session model) can't pass spuriously.
	if len(models) != 4 {
		t.Fatalf("recorded %d requests (%v), want exactly 4 (parent→classifier→child→parent)", len(models), models)
	}
	if models[0] != "gpt-5" {
		t.Fatalf("models[0] (parent) = %q, want the session model gpt-5 (models=%v)", models[0], models)
	}
	if models[1] != "gpt-5" {
		t.Fatalf("models[1] (classifier) = %q, want the session model gpt-5 — no router slot configured (models=%v)", models[1], models)
	}
	if models[2] != routerLarge {
		t.Fatalf("models[2] (routed CHILD) = %q, want the classified model %q (models=%v)", models[2], routerLarge, models)
	}
	if models[3] != "gpt-5" {
		t.Fatalf("models[3] (final parent) = %q, want gpt-5 (models=%v)", models[3], models)
	}
}

// END-TO-END byte-identical-when-OFF: with the router OFF (no flag), the SAME script
// runs without a classifier turn — the child inherits the session model and NO request
// carries a routed model. Proves OFF ⇒ no classifier call.
func TestRouterOffIsByteIdenticalE2E(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	var (
		mu     sync.Mutex
		models []string
	)
	built, err := Build(ctx, Config{
		Workspace:           workspace,
		NoSoul:              true,
		Model:               "gpt-5",
		SubagentModelRouter: false, // OFF
		AllowAllTools:       true,
		envDetector:         fakeEnv(map[string]string{"OPENAI_API_KEY": "sk-x"}),
		liveModelHTTPClient: offlineHTTPClient(),
		providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
			// NO classifier turn: parent → child → parent. If the router fired, the
			// child turn would be consumed by the classifier and the run would desync.
			return mockllm.NewWith([]mockllm.Option{mockllm.WithRequestObserver(func(r port.LLMRequest) {
				mu.Lock()
				models = append(models, r.Model)
				mu.Unlock()
			})},
				mockllm.ToolCallTurn(session.NewToolCall("c1", "Subagent", []byte(`{"prompt":"explore"}`))),
				mockllm.TextTurn("CHILD SUMMARY"),
				mockllm.TextTurn("parent done"),
			)
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()

	sess, err := built.Service.CreateSession(ctx, workspace, session.ModeDefault, defaultLimits())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := built.Service.StartRun(ctx, sess.ID, "go")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got := drainRun(run); got != "parent done" {
		t.Fatalf("terminal text = %q, want parent done (a fired classifier would desync the shared cursor)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// Exactly 3 requests (parent, child, parent) — no classifier turn was consumed.
	if len(models) != 3 {
		t.Fatalf("recorded %d requests (%v), want exactly 3 (parent→child→parent, no classifier call when OFF)", len(models), models)
	}
	for _, m := range models {
		if m != "gpt-5" {
			t.Fatalf("a request carried %q with the router OFF; every request must be the session model gpt-5 (models=%v)", m, models)
		}
	}
}
