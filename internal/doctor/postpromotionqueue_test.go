package doctor_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/doctor"
)

// createQueueDB creates an in-memory (or file) SQLite DB with the post_promotion_queue schema.
func createQueueDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createQueueDB: open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE post_promotion_queue (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		promotion_id TEXT NOT NULL DEFAULT '',
		repo_id TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT '',
		git_sha TEXT NOT NULL DEFAULT '',
		work_kind TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		enqueued_at INTEGER NOT NULL DEFAULT 0,
		completed_at INTEGER,
		error TEXT
	)`)
	if err != nil {
		t.Fatalf("createQueueDB: create table: %v", err)
	}
	return db
}

// TestCheckPostPromotionQueueHealthy verifies an empty queue reports healthy.
func TestCheckPostPromotionQueueHealthy(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createQueueDB(t, path)
	db.Close()

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}
	if len(report.Counts) != 0 {
		t.Errorf("Counts: got %d rows, want 0", len(report.Counts))
	}
	if len(report.FailedRows) != 0 {
		t.Errorf("FailedRows: got %d rows, want 0", len(report.FailedRows))
	}
}

// TestCheckPostPromotionQueueCounts verifies counts are grouped correctly by state×work_kind.
func TestCheckPostPromotionQueueCounts(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createQueueDB(t, path)

	inserts := []struct {
		state    string
		workKind string
	}{
		{"pending", "embed"},
		{"pending", "embed"},
		{"pending", "auto_link"},
		{"in_progress", "embed"},
		{"done", "revalidate"},
		{"done", "revalidate"},
		{"done", "review"},
	}
	for _, r := range inserts {
		_, err := db.Exec(
			`INSERT INTO post_promotion_queue (work_kind, state) VALUES (?, ?)`,
			r.workKind, r.state,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}

	// Build a lookup map for easy assertion.
	type key struct{ state, workKind string }
	got := make(map[key]int)
	for _, c := range report.Counts {
		got[key{c.State, c.WorkKind}] = c.Count
	}

	want := map[key]int{
		{"pending", "embed"}:     2,
		{"pending", "auto_link"}: 1,
		{"in_progress", "embed"}: 1,
		{"done", "revalidate"}:   2,
		{"done", "review"}:       1,
	}
	for k, wantCount := range want {
		if got[k] != wantCount {
			t.Errorf("count[%s/%s]: got %d, want %d", k.state, k.workKind, got[k], wantCount)
		}
	}
	if len(report.Counts) != len(want) {
		t.Errorf("Counts length: got %d, want %d", len(report.Counts), len(want))
	}
}

// TestCheckPostPromotionQueueDegraded verifies a failed row yields Status=degraded.
func TestCheckPostPromotionQueueDegraded(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createQueueDB(t, path)

	_, err := db.Exec(
		`INSERT INTO post_promotion_queue (repo_id, branch, work_kind, state, attempts, error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"repo-abc", "main", "embed", "failed", 3, "connection refused",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "degraded" {
		t.Errorf("Status: got %q, want %q", report.Status, "degraded")
	}
	if len(report.FailedRows) != 1 {
		t.Fatalf("FailedRows: got %d, want 1", len(report.FailedRows))
	}
	fr := report.FailedRows[0]
	if fr.RepoID != "repo-abc" {
		t.Errorf("FailedRow.RepoID: got %q, want %q", fr.RepoID, "repo-abc")
	}
	if fr.Branch != "main" {
		t.Errorf("FailedRow.Branch: got %q, want %q", fr.Branch, "main")
	}
	if fr.WorkKind != "embed" {
		t.Errorf("FailedRow.WorkKind: got %q, want %q", fr.WorkKind, "embed")
	}
	if fr.Attempts != 3 {
		t.Errorf("FailedRow.Attempts: got %d, want 3", fr.Attempts)
	}
	if fr.Error != "connection refused" {
		t.Errorf("FailedRow.Error: got %q, want %q", fr.Error, "connection refused")
	}
}

// TestCheckPostPromotionQueueBroken verifies a non-existent DB path yields Status=broken.
func TestCheckPostPromotionQueueBroken(t *testing.T) {
	path := t.TempDir() + "/nonexistent/veska.db"

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q", report.Status, "broken")
	}
}

// addFindingsTable adds the findings table to a queue test DB so the
// review-pipeline-failure companion-finding invariant can be exercised.
func addFindingsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE findings (
		finding_id    TEXT NOT NULL,
		branch        TEXT NOT NULL,
		repo_id       TEXT NOT NULL,
		node_id       TEXT,
		file_path     TEXT,
		severity      TEXT NOT NULL,
		source_layer  TEXT NOT NULL,
		rule          TEXT NOT NULL,
		message       TEXT NOT NULL,
		state         TEXT NOT NULL,
		closed_reason TEXT,
		created_at    INTEGER NOT NULL,
		closed_at     INTEGER,
		actor_id      TEXT NOT NULL,
		actor_kind    TEXT NOT NULL,
		PRIMARY KEY (finding_id, branch)
	)`)
	if err != nil {
		t.Fatalf("addFindingsTable: %v", err)
	}
}

// TestCheckPostPromotionQueue_FailedReviewNoFinding verifies AC3: a failed
// review row with no open companion review-pipeline-failure finding makes the
// probe report Status=broken (exit 2).
func TestCheckPostPromotionQueue_FailedReviewNoFinding(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createQueueDB(t, path)
	addFindingsTable(t, db)

	_, err := db.Exec(
		`INSERT INTO post_promotion_queue (repo_id, branch, git_sha, work_kind, state, attempts, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"repo-abc", "main", "sha-1", "review", "failed", 3, "model down",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q (failed review row has no companion finding)", report.Status, "broken")
	}
}

// TestCheckPostPromotionQueue_FailedReviewWithFinding verifies AC3's expected
// sticky state: a failed review row WITH an open companion finding is the
// designed parked state and must NOT escalate to broken.
func TestCheckPostPromotionQueue_FailedReviewWithFinding(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createQueueDB(t, path)
	addFindingsTable(t, db)

	const repoID, branch, gitSHA = "repo-abc", "main", "sha-1"
	if _, err := db.Exec(
		`INSERT INTO post_promotion_queue (repo_id, branch, git_sha, work_kind, state, attempts, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		repoID, branch, gitSHA, "review", "failed", 3, "model down",
	); err != nil {
		t.Fatalf("insert queue row: %v", err)
	}
	fid := doctor.ReviewFailureFindingID(repoID, branch, gitSHA)
	if _, err := db.Exec(
		`INSERT INTO findings
			(finding_id, branch, repo_id, node_id, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		 VALUES (?, ?, ?, ?, 'high', 'quality', 'review-pipeline-failure', 'failed', 'open', 0, 'service:veska', 'system')`,
		fid, branch, repoID, gitSHA,
	); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	db.Close()

	report, err := doctor.CheckPostPromotionQueue(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status == "broken" {
		t.Errorf("Status: got broken, want non-broken (failed review row with open companion finding is the expected sticky state)")
	}
}
