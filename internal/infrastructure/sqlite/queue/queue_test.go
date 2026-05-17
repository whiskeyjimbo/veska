package queue_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
)

// openTestDB opens a fully-migrated DB using OpenWithOptions with a temp backup dir.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(dir, "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertPendingRow inserts a row directly into post_promotion_queue for testing.
func insertPendingRow(t *testing.T, db *sql.DB, kind queue.WorkKind) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		"promo-1", "repo-1", "main", "abc123", string(kind), `{"key":"val"}`, time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insertPendingRow: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// rowState reads the state and attempts of a row by seq.
func rowState(t *testing.T, db *sql.DB, seq int64) (state string, attempts int) {
	t.Helper()
	err := db.QueryRow(`SELECT state, attempts FROM post_promotion_queue WHERE seq=?`, seq).
		Scan(&state, &attempts)
	if err != nil {
		t.Fatalf("rowState seq=%d: %v", seq, err)
	}
	return state, attempts
}

// TestMigration0002_TableAndIndexExist verifies migration 0002 creates the table and index.
func TestMigration0002_TableAndIndexExist(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)

	var tblCnt int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='post_promotion_queue'`).Scan(&tblCnt)
	if err != nil {
		t.Fatalf("check table: %v", err)
	}
	if tblCnt != 1 {
		t.Errorf("expected post_promotion_queue table to exist, got count=%d", tblCnt)
	}

	var idxCnt int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_post_promotion_queue_state'`).Scan(&idxCnt)
	if err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCnt != 1 {
		t.Errorf("expected idx_post_promotion_queue_state index to exist, got count=%d", idxCnt)
	}
}

// handlerFunc is a test WorkHandler backed by a function.
type handlerFunc struct {
	fn func(ctx context.Context, row queue.Row) error
}

func (h *handlerFunc) Handle(ctx context.Context, row queue.Row) error {
	return h.fn(ctx, row)
}

// TestPoller_PicksUpPendingRow verifies the poller picks up a pending row, calls the handler,
// and transitions state to done.
func TestPoller_PicksUpPendingRow(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	seq := insertPendingRow(t, db, queue.WorkKindEmbed)

	called := make(chan queue.Row, 1)
	h := &handlerFunc{fn: func(ctx context.Context, row queue.Row) error {
		called <- row
		return nil
	}}

	handlers := map[queue.WorkKind]queue.WorkHandler{
		queue.WorkKindEmbed: h,
	}
	p := queue.New(db, db, handlers)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p.Start(ctx)

	select {
	case row := <-called:
		if row.Seq != seq {
			t.Errorf("handler got seq=%d, want %d", row.Seq, seq)
		}
		if row.Kind != queue.WorkKindEmbed {
			t.Errorf("handler got kind=%q, want embed", row.Kind)
		}
	case <-ctx.Done():
		t.Fatal("timeout: handler was never called")
	}

	// Wait briefly for state update to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := rowState(t, db, seq)
		if state == "done" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	state, _ := rowState(t, db, seq)
	t.Errorf("expected state=done, got %q", state)
}

// TestPoller_RetryAndFail verifies that a handler error increments attempts,
// re-queues as pending, and after 3 attempts transitions to failed.
func TestPoller_RetryAndFail(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	seq := insertPendingRow(t, db, queue.WorkKindAutoLink)

	var callCount atomic.Int32
	h := &handlerFunc{fn: func(ctx context.Context, row queue.Row) error {
		callCount.Add(1)
		return errors.New("handler error")
	}}

	handlers := map[queue.WorkKind]queue.WorkHandler{
		queue.WorkKindAutoLink: h,
	}

	// Use a shorter interval for faster test execution.
	p := queue.NewWithInterval(db, db, handlers, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.Start(ctx)

	// Wait until state=failed (after 3 attempts).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		state, attempts := rowState(t, db, seq)
		if state == "failed" && attempts >= 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	state, attempts := rowState(t, db, seq)
	t.Errorf("expected state=failed with attempts>=3, got state=%q attempts=%d", state, attempts)
}

// TestPoller_ContextCancelStopsGoroutines verifies that cancelling the context
// causes Start to stop cleanly without goroutine leaks.
func TestPoller_ContextCancelStopsGoroutines(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)

	handlers := map[queue.WorkKind]queue.WorkHandler{
		queue.WorkKindEmbed:      &handlerFunc{fn: func(_ context.Context, _ queue.Row) error { return nil }},
		queue.WorkKindAutoLink:   &handlerFunc{fn: func(_ context.Context, _ queue.Row) error { return nil }},
		queue.WorkKindRevalidate: &handlerFunc{fn: func(_ context.Context, _ queue.Row) error { return nil }},
		queue.WorkKindReview:     &handlerFunc{fn: func(_ context.Context, _ queue.Row) error { return nil }},
	}

	p := queue.NewWithInterval(db, db, handlers, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Let it run briefly.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait() blocks until all goroutines exit.
	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("goroutines did not stop after context cancel within 2s")
	}
}

// TestPoller_NoHandlerRowSkipped verifies that a row with no registered handler is left pending.
func TestPoller_NoHandlerRowSkipped(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	// Insert a row for WorkKindReview but register no handler for it.
	seq := insertPendingRow(t, db, queue.WorkKindReview)

	// Only register embed handler; review has no handler.
	handlers := map[queue.WorkKind]queue.WorkHandler{
		queue.WorkKindEmbed: &handlerFunc{fn: func(_ context.Context, _ queue.Row) error { return nil }},
	}

	p := queue.NewWithInterval(db, db, handlers, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	p.Start(ctx)
	<-ctx.Done()

	// The review row should still be pending because no handler was registered for it.
	state, _ := rowState(t, db, seq)
	if state != "pending" {
		t.Errorf("expected state=pending (no handler registered), got %q", state)
	}
}

// TestPoller_WikiLaneDrains verifies the WorkKindWiki lane: a pending wiki
// row is picked up by its handler and transitions to state=done.
func TestPoller_WikiLaneDrains(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	seq := insertPendingRow(t, db, queue.WorkKindWiki)

	called := make(chan queue.Row, 1)
	h := &handlerFunc{fn: func(_ context.Context, row queue.Row) error {
		called <- row
		return nil
	}}
	handlers := map[queue.WorkKind]queue.WorkHandler{
		queue.WorkKindWiki: h,
	}
	p := queue.NewWithInterval(db, db, handlers, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.Start(ctx)

	select {
	case row := <-called:
		if row.Kind != queue.WorkKindWiki {
			t.Errorf("handler got kind=%q, want wiki", row.Kind)
		}
	case <-ctx.Done():
		t.Fatal("timeout: wiki handler was never called")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state, _ := rowState(t, db, seq); state == "done" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	state, _ := rowState(t, db, seq)
	t.Errorf("expected wiki row state=done, got %q", state)
}
