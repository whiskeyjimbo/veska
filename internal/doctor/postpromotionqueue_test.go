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
