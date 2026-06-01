package daemon

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// runGit shells `git` in dir, failing the test on non-zero exit.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// initGitRepoWithGoFile lays out a temp git repo containing one committed
// .go file. The .git/hooks directory is created so repo.Add (which installs
// post-commit / post-checkout hooks) does not fail when called against it.
func initGitRepoWithGoFile(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	src := "package sample\n\nfunc Hello() string { return \"hi\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write sample.go: %v", err)
	}
	runGit(t, dir, "add", "sample.go")
	runGit(t, dir, "commit", "-q", "-m", "init")

	// Hooks directory exists by default after `git init`, but be explicit
	// in case future hardening removes it from the test fixture.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	return dir
}

// TestDaemon_ResyncWired_FieldPresent is a smoke test for the wiring change:
// newDaemon must populate d.resync so Start can spawn the resync goroutine.
// A nil field means the wiring was dropped.
func TestDaemon_ResyncWired_FieldPresent(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if d.resync == nil {
		t.Fatal("d.resync is nil after newDaemon; wiring missing")
	}
}

// TestDaemon_StartupResync_NeverPromoted_Reparses exercises AC1 at the
// wiring level: a repo with last_promoted_sha = "" is routed through the
// reparser when StartupResync.Run is invoked.
//
// We use the daemon's real repoLister (so the wiring path is exercised)
// but swap a spy in for the reparser. The full real-pipeline path
// (Ingester.Save → Promoter.Promote → SQLite nodes) is gated on the
// cold-scan integration follow-up bead (see TestColdScanReparser_Integration
// in coldscan_test.go, currently skipped under solov2-21h).
func TestDaemon_StartupResync_NeverPromoted_Reparses(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	gitDir := initGitRepoWithGoFile(t)
	repoID, _, err := repo.Add(context.Background(), d.pools.Write, gitDir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	// last_promoted_sha defaults to NULL after repo.Add, which RepoLister
	// flattens to "" — the never-promoted route.

	called := make(map[string]int)
	var mu sync.Mutex
	spy := func(_ context.Context, rec application.RepoRecord) error {
		mu.Lock()
		defer mu.Unlock()
		called[rec.RepoID]++
		return nil
	}

	resync, _ := application.NewStartupResync(
		&repoLister{db: d.pools.ReadDB},
		gitwatch.Querier{}, d.ingester.Save, d.promoter.Promote, spy,
	)
	if err := resync.Run(context.Background()); err != nil {
		t.Fatalf("resync.Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if called[repoID] != 1 {
		t.Fatalf("reparser calls for %q = %d; want 1", repoID, called[repoID])
	}
}

// TestDaemon_StartupResync_AtHEAD_SkipsReparse exercises AC2: a repo
// whose last_promoted_sha already equals HEAD takes the cheap path —
// the reparser is NOT invoked. As with AC1 this is asserted via a spy
// over the daemon's wiring (real repoLister + real Querier).
func TestDaemon_StartupResync_AtHEAD_SkipsReparse(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	gitDir := initGitRepoWithGoFile(t)
	repoID, _, err := repo.Add(context.Background(), d.pools.Write, gitDir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	// Resolve HEAD and pre-stamp it as the last promoted SHA.
	headOut, err := exec.Command("git", "-C", gitDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	head := string(headOut)
	head = head[:len(head)-1] // strip newline
	if _, err := d.pools.Write.Exec(
		`UPDATE repos SET last_promoted_sha = ?, active_branch = 'main' WHERE repo_id = ?`,
		head, repoID,
	); err != nil {
		t.Fatalf("stamp last_promoted_sha: %v", err)
	}

	called := make(map[string]int)
	var mu sync.Mutex
	spy := func(_ context.Context, rec application.RepoRecord) error {
		mu.Lock()
		defer mu.Unlock()
		called[rec.RepoID]++
		return nil
	}

	resync, _ := application.NewStartupResync(
		&repoLister{db: d.pools.ReadDB},
		gitwatch.Querier{}, d.ingester.Save, d.promoter.Promote, spy,
	)
	if err := resync.Run(context.Background()); err != nil {
		t.Fatalf("resync.Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if called[repoID] != 0 {
		t.Fatalf("reparser called for at-HEAD repo %q %d times; want 0 (cheap path)",
			repoID, called[repoID])
	}
}

// TestDaemon_StartupResync_FullPipeline exercises the full daemon-wired
// pipeline: newDaemon's cold-scan reparser closure → real Ingester → real
// Promoter → SQLite nodes. We invoke d.resync.Run synchronously so the
// assertion is not racing the resync goroutine that Start would have spawned.
// The companion application-layer integration test
// (TestColdScanReparser_Integration_RealPipeline) verifies the pipeline in
// isolation; this test verifies it through the daemon's actual wiring.
func TestDaemon_StartupResync_FullPipeline(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	gitDir := initGitRepoWithGoFile(t)
	repoID, _, err := repo.Add(context.Background(), d.pools.Write, gitDir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	// Drive the daemon's real resync (with the daemon's real cold-scan
	// reparser closure) synchronously — no goroutine race.
	if err := d.resync.Run(context.Background()); err != nil {
		t.Fatalf("d.resync.Run: %v", err)
	}

	var nodeCount int
	if err := d.pools.ReadDB.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE repo_id = ?`, repoID,
	).Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount < 1 {
		t.Fatalf("nodes for repo %q after full-pipeline resync: got %d, want >= 1",
			repoID, nodeCount)
	}

	// Post-promotion queue rows must exist (one per work_kind × file + wiki).
	var queueCount int
	if err := d.pools.ReadDB.QueryRow(
		`SELECT COUNT(*) FROM post_promotion_queue WHERE repo_id = ?`, repoID,
	).Scan(&queueCount); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if queueCount == 0 {
		t.Error("post_promotion_queue: got 0 rows for repo; promotion sinks did not run")
	}

	// repos.last_promoted_sha must advance to HEAD — the promotion
	// transaction writes it atomically with the node rows .
	headOut, err := exec.Command("git", "-C", gitDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	wantSHA := string(headOut[:len(headOut)-1])

	var gotSHA string
	if err := d.pools.ReadDB.QueryRow(
		`SELECT COALESCE(last_promoted_sha, '') FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&gotSHA); err != nil {
		t.Fatalf("read last_promoted_sha: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("last_promoted_sha = %q, want %q", gotSHA, wantSHA)
	}

	// End-to-end demonstration of the c47 fix: re-running resync now takes
	// the cheap path. We re-route the reparser through a spy and assert it
	// is NOT invoked, because LastPromotedSHA == HEAD on the second pass.
	spyCalls := 0
	cheapResync, _ := application.NewStartupResync(
		&repoLister{db: d.pools.ReadDB},
		gitwatch.Querier{}, d.ingester.Save, d.promoter.Promote,
		func(context.Context, application.RepoRecord) error { spyCalls++; return nil },
	)
	if err := cheapResync.Run(context.Background()); err != nil {
		t.Fatalf("cheap-path resync: %v", err)
	}
	if spyCalls != 0 {
		t.Errorf("second resync invoked reparser %d times; want 0 (cheap path expected after c47 fix)", spyCalls)
	}
}

// TestDaemon_VectorStoreRehydratesOnSecondStart covers solov2-249: the
// in-memory vector store is repopulated from node_embeddings on a fresh
// Daemon.Start. We simulate "daemon restart" by running newDaemon twice
// over the same VESKA_HOME; the second instance's vector store is empty
// before Start, then Start triggers the rehydrate path and the count
// reaches the persisted ready-ref count.
func TestDaemon_VectorStoreRehydratesOnSecondStart(t *testing.T) {
	cfg := testConfig(t)

	// First daemon: register a repo + drive a cold scan so node_embeddings
	// gets populated. We don't need a real embedder run — directly seed
	// the durable tables via the same daemon's write pool.
	d1, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon #1: %v", err)
	}

	gitDir := initGitRepoWithGoFile(t)
	repoID, _, err := repo.Add(context.Background(), d1.pools.Write, gitDir)
	if err != nil {
		_ = d1.Stop()
		t.Fatalf("repo.Add: %v", err)
	}
	if err := d1.resync.Run(context.Background()); err != nil {
		_ = d1.Stop()
		t.Fatalf("d1.resync.Run: %v", err)
	}

	// Seed a single known embedding directly into the durable tables — the
	// real embedder requires Ollama which we don't have in unit tests.
	// The vec is L2-normalised (magnitude 1) so any score comparison
	// downstream behaves as the production code expects.
	vec := []float32{1, 0, 0}
	blob := encodeVecLE(vec)
	const hash = "h-test-rehydrate"
	const model = "m-test"
	if _, err := d1.pools.Write.Exec(
		`INSERT INTO node_embeddings (content_hash, model, dim, embedding, created_at) VALUES (?, ?, ?, ?, 0)`,
		hash, model, len(vec), blob,
	); err != nil {
		_ = d1.Stop()
		t.Fatalf("seed node_embeddings: %v", err)
	}
	// Pick the first node we just promoted and point its ref at the seeded hash.
	var nodeID string
	if err := d1.pools.ReadDB.QueryRow(
		`SELECT node_id FROM nodes WHERE repo_id = ? LIMIT 1`, repoID,
	).Scan(&nodeID); err != nil {
		_ = d1.Stop()
		t.Fatalf("lookup node_id: %v", err)
	}
	if _, err := d1.pools.Write.Exec(
		`UPDATE node_embedding_refs SET state='ready', content_hash=? WHERE node_id=?`,
		hash, nodeID,
	); err != nil {
		_ = d1.Stop()
		t.Fatalf("mark ref ready: %v", err)
	}

	// Stop the first daemon. The in-memory sqlite-vec contents are gone.
	if err := d1.Stop(); err != nil {
		t.Fatalf("d1.Stop: %v", err)
	}

	// Second daemon — same VESKA_HOME, fresh sqlite-vec. Search before Start
	// returns nothing (memory empty). Start triggers rehydrate and Search
	// returns the seeded row.
	d2, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon #2: %v", err)
	}
	t.Cleanup(func() { _ = d2.Stop() })

	preHits, err := d2.vectors.Search(context.Background(), repoID, "main", vec, 5, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("pre-Start search: %v", err)
	}
	if len(preHits) != 0 {
		t.Errorf("pre-Start vector store unexpectedly non-empty: %+v", preHits)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d2.Start(ctx); err != nil {
		t.Fatalf("d2.Start: %v", err)
	}

	hits, err := d2.vectors.Search(context.Background(), repoID, "main", vec, 5, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("post-Start search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.NodeID == nodeID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rehydrated vector store does not contain seeded node %q; hits=%+v", nodeID, hits)
	}
}

// encodeVecLE mirrors embedder.encodeFloat32LE for the rehydrate test —
// duplicated locally so this test does not depend on an unexported helper.
func encodeVecLE(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		bits := math.Float32bits(v)
		buf[i*4+0] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}

// TestDaemon_StartupResync_StartDoesNotBlock guards the epic constraint
// that the scan must not block Start. We hold-up the resync by registering
// no repos (cheap path) and confirm Start returns well under the resync's
// nominal completion window — the goroutine fans out on its own.
func TestDaemon_StartupResync_StartDoesNotBlock(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	elapsed := time.Since(start)
	// Start has to bind sockets etc.; 2s is generous but still well below
	// any plausible resync wall-time on a real repo set.
	if elapsed > 2*time.Second {
		t.Errorf("Start blocked for %v; expected non-blocking", elapsed)
	}
}
