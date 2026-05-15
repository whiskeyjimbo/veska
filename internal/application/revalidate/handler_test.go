package revalidate_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// ── fake repo ──────────────────────────────────────────────────────────────

type fakeRepo struct {
	stale      []ports.StaleFinding
	staleErr   error
	closeErr   error
	closedIDs  []string
	closedAt   []int64
	queryCalls int
}

func (f *fakeRepo) StaleFindingsForFile(_ context.Context, _, _, _ string) ([]ports.StaleFinding, error) {
	f.queryCalls++
	if f.staleErr != nil {
		return nil, f.staleErr
	}
	return f.stale, nil
}

func (f *fakeRepo) CloseAsRevalidatedObsolete(_ context.Context, _, _, findingID string, closedAt int64) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closedIDs = append(f.closedIDs, findingID)
	f.closedAt = append(f.closedAt, closedAt)
	return nil
}

// ── unit tests against the fake ────────────────────────────────────────────

func TestHandler_RejectsWrongKind(t *testing.T) {
	t.Parallel()
	h := revalidate.NewHandler(&fakeRepo{})
	err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindEmbed, Payload: "x.go"})
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

func TestHandler_EmptyPayloadIsNoop(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := revalidate.NewHandler(repo)
	err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindRevalidate, Payload: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.queryCalls != 0 {
		t.Errorf("expected zero query calls for empty payload, got %d", repo.queryCalls)
	}
}

func TestHandler_NoStaleFindingsIsNoop(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{stale: nil}
	h := revalidate.NewHandler(repo)
	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.closedIDs) != 0 {
		t.Errorf("expected no closes, got %v", repo.closedIDs)
	}
}

func TestHandler_StaleQueryErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-query")
	repo := &fakeRepo{staleErr: sentinel}
	h := revalidate.NewHandler(repo)
	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_CloseErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-close")
	repo := &fakeRepo{
		stale:    []ports.StaleFinding{{FindingID: "fA", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"}},
		closeErr: sentinel,
	}
	h := revalidate.NewHandler(repo)
	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_ClosesEachStaleFindingWithFixedTimestamp(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", NodeID: "n1", AnchorHash: "h-a-old", CurrentHash: "h-a-new"},
			{FindingID: "fB", NodeID: "n2", AnchorHash: "h-b-old", CurrentHash: "h-b-new"},
		},
	}
	fixed := time.Unix(1700000000, 0)
	h := revalidate.NewHandler(repo, revalidate.WithClock(func() time.Time { return fixed }))

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 2 {
		t.Fatalf("close count = %d, want 2 (%v)", len(repo.closedIDs), repo.closedIDs)
	}
	if repo.closedIDs[0] != "fA" || repo.closedIDs[1] != "fB" {
		t.Errorf("close order = %v, want [fA fB]", repo.closedIDs)
	}
	wantMillis := fixed.UnixMilli()
	for i, ts := range repo.closedAt {
		if ts != wantMillis {
			t.Errorf("closedAt[%d] = %d, want %d", i, ts, wantMillis)
		}
	}
}

func TestHandler_MetricsIncrementPerClose(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA"}, {FindingID: "fB"}, {FindingID: "fC"},
		},
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := testutil.ToFloat64(metrics.RevalidateClosed)
	if got != 3 {
		t.Errorf("veska_revalidate_closed_total = %v, want 3", got)
	}
}

func TestHandler_MetricsNotIncrementedOnNoStale(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{stale: nil}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))

	_ = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 0 {
		t.Errorf("counter = %v, want 0", got)
	}
}

func TestHandler_NilMetricsIsFunctional(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{{FindingID: "fA"}},
	}
	h := revalidate.NewHandler(repo, revalidate.WithMetrics(nil))
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 {
		t.Errorf("closed = %v, want [fA]", repo.closedIDs)
	}
}

func TestNewHandler_NilRepoPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil repo")
		}
	}()
	_ = revalidate.NewHandler(nil)
}

// ── integration test against real *sql.DB ──────────────────────────────────

// TestHandler_Integration_ClosesOnlyStaleFinding wires the real SQLite adapter
// behind the handler. Two findings on the same file: one whose anchor hash
// matches current content (must stay open), one whose anchor drifted (must
// close with revalidated_obsolete).
func TestHandler_Integration_ClosesOnlyStaleFinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "v.db"))

	insertRepo(t, db, "repo1")
	// Two nodes on the same file. n-fresh's current hash matches what we'll
	// record on the finding; n-stale's current hash diverges from the
	// finding's anchor hash.
	insertNode(t, db, "n-fresh", "repo1", "main", "pkg/a.go", "h-fresh")
	insertNode(t, db, "n-stale", "repo1", "main", "pkg/a.go", "h-current")

	findRepo := sqlite.NewFindingRepo(db)
	revalRepo := sqlite.NewRevalidateRepo(db)

	mustFinding := func(id, nodeID, hash string) *domain.Finding {
		t.Helper()
		f, err := domain.NewFinding(
			id, "repo1", "main",
			domain.SeverityLow, domain.LayerStructural,
			"dead-code", "msg-"+id,
			domain.WithNodeAnchor(nodeID),
			domain.WithAnchorContentHash(hash),
		)
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		return f
	}

	fFresh := mustFinding("u-fresh", "n-fresh", "h-fresh")      // matches
	fStale := mustFinding("u-stale", "n-stale", "h-anchor-old") // drift

	if err := findRepo.Save(context.Background(), fFresh); err != nil {
		t.Fatalf("Save fresh: %v", err)
	}
	if err := findRepo.Save(context.Background(), fStale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h := revalidate.NewHandler(revalRepo, revalidate.WithMetrics(metrics))

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "repo1", Branch: "main", Payload: "pkg/a.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	type rowState struct {
		state, reason string
	}
	get := func(id, branch string) rowState {
		t.Helper()
		var rs rowState
		var reason sql.NullString
		if err := db.QueryRow(
			`SELECT state, closed_reason FROM findings WHERE finding_id = ? AND branch = ?`,
			id, branch,
		).Scan(&rs.state, &reason); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if reason.Valid {
			rs.reason = reason.String
		}
		return rs
	}

	if got := get(fFresh.FindingID, fFresh.Branch); got.state != "open" {
		t.Errorf("fresh finding state = %q, want open (reason=%q)", got.state, got.reason)
	}
	gotStale := get(fStale.FindingID, fStale.Branch)
	if gotStale.state != "closed" {
		t.Errorf("stale finding state = %q, want closed", gotStale.state)
	}
	if gotStale.reason != "revalidated_obsolete" {
		t.Errorf("stale finding closed_reason = %q, want revalidated_obsolete", gotStale.reason)
	}
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 1 {
		t.Errorf("counter = %v, want 1", got)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func openTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertRepo(t *testing.T, db *sql.DB, repoID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/"+repoID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo %s: %v", repoID, err)
	}
}

func insertNode(t *testing.T, db *sql.DB, nodeID, repoID, branch, filePath, contentHash string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, branch, repoID, "go", "function", nodeID, filePath,
		1, 10, contentHash, time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}
