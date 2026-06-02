package daemon

// In-process MCP tool-coverage harness (solov2-ti9x).
//
// This is the white-box test substrate the 40 per-tool coverage beads build on.
// It reuses the REAL wiring: it hand-builds an mcpDeps over a freshly indexed
// golden fixture and calls the unexported registerMCPTools — it does NOT
// replicate registration and does NOT call newDaemon (which would start the
// fsnotify watcher, the socket server, and the async embedder worker, adding
// timing nondeterminism unwanted in CI).
//
// State isolation is by construction: newHarness performs a full fresh setup
// (own temp DB + own in-memory vector store + deterministic static embed) every
// call, so the ~11 mutating tools cannot leak state across subtests and the
// suite is order-independent. There is intentionally NO shared-golden cache:
// every coverage subtest currently t.Skips, so nothing pays setup cost yet. A
// reader-side cached golden may be added later if setup time becomes a concern.
//
// Indexing walks the SHARED read-only fixture root (a read-only walk is safe
// across parallel instances) rather than copying the source per instance, so
// node IDs — which embed the absolute walked path — are identical across every
// harness instance. Only the DB and vector store are per-instance.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// harnessOptions captures the documented opt-ins for newHarness.
type harnessOptions struct {
	// withTaskTools additionally registers the 3 PARKED task tools
	// (eng_set_active_task / eng_get_active_task / eng_get_task_history) via
	// mcp.RegisterTaskTools, so the 3 task beads can cover them. The default
	// registry stays faithful to the 37 tools production wires.
	withTaskTools bool
}

// HarnessOption configures newHarness.
type HarnessOption func(*harnessOptions)

// WithTaskTools opts the harness into registering the 3 parked task tools.
// Documented opt-in: only the 3 task coverage beads should pass it.
func WithTaskTools() HarnessOption {
	return func(o *harnessOptions) { o.withTaskTools = true }
}

// toolHarness is a fully wired, isolated MCP tool surface over the golden
// fixture. Construct one per subtest via newHarness; it is never shared.
type toolHarness struct {
	t     *testing.T
	reg   *mcp.Registry
	pools *sqlite.Pools

	// roots maps each fixture repoID to the absolute root it was indexed at.
	// Per the package doc this is the SHARED fixture root, identical across
	// instances, so resolved node IDs are stable.
	roots map[string]string
}

// fixtureRepo pairs a repoID with its module path and on-disk module root.
type fixtureRepo struct {
	repoID     string
	modulePath string
	root       string
}

// fixtureRepos returns the two golden-fixture repos keyed to the shared
// testdata roots resolved via the coverage package's runtime-anchored locator.
func fixtureRepos() []fixtureRepo {
	return []fixtureRepo{
		{coverage.AlphaRepoID, coverage.AlphaModulePath, coverage.ModuleRoot("modalpha")},
		{coverage.BetaRepoID, coverage.BetaModulePath, coverage.ModuleRoot("modbeta")},
	}
}

// newHarness builds an isolated tool surface: fresh temp DB + fresh in-memory
// vector store + the golden fixture indexed and embedded to completion, with
// operational seed-state inserted. It then hand-builds mcpDeps and calls the
// real registerMCPTools. Setup failures are fatal.
func newHarness(t *testing.T, opts ...HarnessOption) *toolHarness {
	t.Helper()

	var cfg harnessOptions
	for _, o := range opts {
		o(&cfg)
	}

	pools := openHarnessPools(t)
	vectors, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
	if err != nil {
		t.Fatalf("harness: vector.NewVectorStorage: %v", err)
	}
	provider, err := static.New()
	if err != nil {
		t.Fatalf("harness: static.New: %v", err)
	}

	h := &toolHarness{t: t, pools: pools, roots: map[string]string{}}

	ingester, promoter, reparser := h.buildPipeline()
	for _, fr := range fixtureRepos() {
		h.roots[fr.repoID] = fr.root
		h.indexRepo(reparser, fr)
	}
	h.drainEmbeddings(provider, vectors)
	h.seedOperationalState()

	h.reg = mcp.NewRegistry()
	deps := mcpDeps{
		pools:    pools,
		cfg:      Config{},
		staging:  staging.NewArea(),
		vectors:  vectors,
		provider: provider,
		refs:     sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write),
		ingester: ingester,
		promoter: promoter,
		reparser: reparser,
	}
	if err := registerMCPTools(h.reg, deps); err != nil {
		t.Fatalf("harness: registerMCPTools: %v", err)
	}
	if cfg.withTaskTools {
		mcp.RegisterTaskTools(h.reg, sqlite.NewTaskRepo(pools.Write), nil)
	}
	return h
}

// openHarnessPools opens a fresh temp DB through the production migrations and
// returns read/write pools for it, closed on test cleanup.
func openHarnessPools(t *testing.T) *sqlite.Pools {
	t.Helper()
	dbPath := t.TempDir() + "/coverage.sqlite"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("harness: sqlite.Open: %v", err)
	}
	_ = db.Close() // Open runs migrations; reopen via pools for the two-handle model.
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("harness: sqlite.OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// buildPipeline assembles the parse→promote→embed-ref chain over the write
// pool, plus a cold-scan reparser. The ingester carries a FindingStorage so
// TODO findings land (later todo-tool beads need them). The returned reparser
// is also handed to mcpDeps so eng_promote_repo / eng_reindex_repo register.
func (h *toolHarness) buildPipeline() (
	*application.Ingester, *application.Promoter,
	func(context.Context, application.RepoRecord) error,
) {
	db := h.pools.Write
	parser := treesitter.NewGoParser()
	area := staging.NewArea()
	gate := staging.NewGate(area)
	ingester := application.NewIngester(parser, area, gate,
		application.WithFindingStorage(sqlite.NewFindingRepo(db)))
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{
		sqlite.NewFTSSink(), sqlite.NewEmbedRefSink(),
	})
	promoter := application.NewPromoter(area, store)
	reparser, err := application.NewColdScanReparser(ingester, promoter, fixtureHeadQuerier{})
	if err != nil {
		h.t.Fatalf("harness: NewColdScanReparser: %v", err)
	}
	return ingester, promoter, reparser
}

// indexRepo registers fr in the repos table (with its real root + module path)
// and cold-scans it synchronously into the DB.
func (h *toolHarness) indexRepo(
	reparser func(context.Context, application.RepoRecord) error, fr fixtureRepo,
) {
	h.t.Helper()
	if _, err := h.pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)`,
		fr.repoID, fr.root, time.Now().UnixMilli(), coverage.FixtureBranch, fr.modulePath,
	); err != nil {
		h.t.Fatalf("harness: insert repos row (%s): %v", fr.repoID, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := reparser(ctx, application.RepoRecord{
		RepoID:       fr.repoID,
		RootPath:     fr.root,
		ActiveBranch: coverage.FixtureBranch,
	}); err != nil {
		h.t.Fatalf("harness: reparser (%s): %v", fr.repoID, err)
	}
}

// drainEmbeddings runs the embedder worker against the static provider until
// every pending node_embedding_ref is consumed, so vector-backed tools query a
// fully populated store. This mirrors the coldscan-e2e drain-then-assert pattern
// but never relies on a background worker outliving setup.
func (h *toolHarness) drainEmbeddings(provider ports.EmbeddingProvider, vectors ports.VectorStorage) {
	h.t.Helper()
	refs := sqlite.NewEmbeddingRefsRepo(h.pools.ReadDB, h.pools.Write)
	worker, err := embedder.NewWorker(refs, provider, vectors, embedder.WithInterval(5*time.Millisecond))
	if err != nil {
		h.t.Fatalf("harness: embedder.NewWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop()
	if err := waitHarnessPendingDrained(ctx, refs); err != nil {
		h.t.Fatalf("harness: embedder drain timed out: %v", err)
	}
}

// waitHarnessPendingDrained polls CountPending until it hits 0 or ctx fires.
func waitHarnessPendingDrained(ctx context.Context, refs *sqlite.EmbeddingRefsRepo) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		n, err := refs.CountPending(ctx)
		if err == nil && n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Call marshals params to JSON, looks up the named tool's handler, and invokes
// it with a deterministic agent actor. It returns the handler's (result, rpcErr)
// untouched so a per-tool bead asserts on either. A nil params marshals to an
// empty body. An unknown tool name is fatal (a coverage bead naming a missing
// tool is a wiring bug, not a tool result).
func (h *toolHarness) Call(toolName string, params any) (any, *mcp.RPCError) {
	h.t.Helper()
	spec, ok := h.reg.Spec(toolName)
	if !ok {
		h.t.Fatalf("harness: tool %q is not registered", toolName)
	}
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			h.t.Fatalf("harness: marshal params for %q: %v", toolName, err)
		}
		raw = b
	}
	return spec.Handler(context.Background(), harnessActor(), raw)
}

// harnessActor is the attribution stamp every harness Call carries.
func harnessActor() domain.Actor {
	return domain.Actor{ID: "tool-coverage-harness", Kind: domain.ActorKindAgent}
}

// Registry exposes the harness's registry so guard tests can read its tool
// names. Callers must not Register onto it after construction.
func (h *toolHarness) Registry() *mcp.Registry { return h.reg }

// RepoIDs returns the fixture repo IDs in a stable order (alpha, beta).
func (h *toolHarness) RepoIDs() []string {
	return []string{coverage.AlphaRepoID, coverage.BetaRepoID}
}

// Root returns the absolute index root the given repo was walked at, for
// callers that must reconstruct an absolute path. Fatal on an unknown repoID.
func (h *toolHarness) Root(repoID string) string {
	h.t.Helper()
	root, ok := h.roots[repoID]
	if !ok {
		h.t.Fatalf("harness: unknown repoID %q", repoID)
	}
	return root
}

// ResolveID maps a manifest NodeKey to the node_id the pipeline emitted for it
// in repoID, using the root this harness indexed at. This is the ONLY supported
// way for a coverage bead to obtain a node ID — no bead pastes a raw sha256.
func (h *toolHarness) ResolveID(repoID string, key coverage.NodeKey) domain.NodeID {
	return key.ResolveID(repoID, h.Root(repoID))
}

// fixtureHeadQuerier is the cold-scan headQuerier stub; the fixture is not a
// real git repo so HEAD is a fixed sentinel.
type fixtureHeadQuerier struct{}

func (fixtureHeadQuerier) HEAD(string) (string, error) { return "sha-fixture", nil }

// execSeed runs a single seed INSERT, failing the test on error. Used by
// seedOperationalState (toolcoverage_seed_test.go).
func (h *toolHarness) execSeed(query string, args ...any) {
	h.t.Helper()
	if _, err := h.pools.Write.Exec(query, args...); err != nil {
		h.t.Fatalf("harness seed: %v\nquery: %s", err, query)
	}
}

// compile-time guard: *sqlite.TaskRepo must satisfy mcp.TaskStore for the
// task-tool opt-in to wire.
var _ mcp.TaskStore = (*sqlite.TaskRepo)(nil)
