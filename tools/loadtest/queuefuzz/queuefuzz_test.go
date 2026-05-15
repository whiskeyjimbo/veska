//go:build eval

// Package queuefuzz drives the M3 gate-5 queue-lane drain fuzz test.
//
// Goal: drive N synthetic promotions through the real Promoter, let the
// real queue Poller (with stub WorkHandlers) drain every enqueued row, and
// assert that every row reaches state='done' (or state='failed' with a
// surfaced reason) within a configurable wall-time budget. No rows must
// remain 'pending' past the budget.
//
// Three M3 lanes are exercised: embed, auto_link, revalidate. The 'review'
// kind is reserved for M5 and never enqueued by Promoter today.
//
// Build-tag gated; the make target is `make eval-queue-fuzz`.
package queuefuzz

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
)

const (
	defaultPromotions = 100
	defaultBudgetMS   = 60_000
	defaultLatencyMS  = 0

	repoID = "queuefuzz-eval"
	branch = "main"
)

// TestQueueFuzz is the M3 gate-5 fuzz drain harness.
func TestQueueFuzz(t *testing.T) {
	promotions := envInt("QUEUEFUZZ_PROMOTIONS", defaultPromotions)
	budgetMS := envInt("QUEUEFUZZ_BUDGET_MS", defaultBudgetMS)
	latencyMS := envInt("QUEUEFUZZ_HANDLER_LATENCY_MS", defaultLatencyMS)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// SQLite pools with full migrations (Promoter requires every table it
	// touches — nodes, edges, post_promotion_queue, node_embedding_refs,
	// node_fts_words, node_fts_trigrams, repos, findings).
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/veska.db"
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("sqlite.OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	seedRepo(t, pools.WriteHot)

	// Stub handlers: succeed for every kind. Optional artificial latency
	// (random 0..latencyMS) exercises real concurrency across lanes.
	stub := newStubHandler(latencyMS)
	handlers := map[queue.WorkKind]queue.WorkHandler{
		ports.WorkKindEmbed:      stub,
		ports.WorkKindAutoLink:   stub,
		ports.WorkKindRevalidate: stub,
	}

	// Tight poll interval keeps the fuzz responsive without hammering the
	// DB. 25ms is well below the budget granularity.
	poller := queue.NewWithInterval(pools.ReadDB, pools.WriteHot, handlers, 25*time.Millisecond)
	poller.Start(ctx)
	t.Cleanup(func() {
		cancel()
		poller.Wait()
	})

	staging := application.NewStagingArea()
	promoter := application.NewPromoter(staging, pools.WriteHot)
	actor := domain.Actor{ID: "service:queuefuzz", Kind: domain.ActorKindSystem}

	start := time.Now()
	drivePromotions(ctx, t, staging, promoter, actor, promotions)
	enqueueElapsed := time.Since(start)

	// Wait for the queue to drain: zero rows in state='pending' OR
	// state='in_progress'. Failed rows are also terminal (handler errors
	// surface in the row's error column).
	budget := time.Duration(budgetMS) * time.Millisecond
	deadline := start.Add(budget)
	budgetMet := waitDrain(t, pools.ReadDB, deadline)

	elapsed := time.Since(start)

	// Tally per-kind outcomes — done / failed / stuck (pending OR in_progress).
	counts := tallyCounts(t, pools.ReadDB)
	expectedPerKind := promotions // 1 file per promotion → 1 row per kind

	out := result{
		Promotions: promotions,
		RowsPerKind: map[string]int{
			"embed":      counts.rows[ports.WorkKindEmbed],
			"auto_link":  counts.rows[ports.WorkKindAutoLink],
			"revalidate": counts.rows[ports.WorkKindRevalidate],
		},
		DonePerKind: map[string]int{
			"embed":      counts.done[ports.WorkKindEmbed],
			"auto_link":  counts.done[ports.WorkKindAutoLink],
			"revalidate": counts.done[ports.WorkKindRevalidate],
		},
		FailedPerKind: map[string]int{
			"embed":      counts.failed[ports.WorkKindEmbed],
			"auto_link":  counts.failed[ports.WorkKindAutoLink],
			"revalidate": counts.failed[ports.WorkKindRevalidate],
		},
		StuckPerKind: map[string]int{
			"embed":      counts.stuck[ports.WorkKindEmbed],
			"auto_link":  counts.stuck[ports.WorkKindAutoLink],
			"revalidate": counts.stuck[ports.WorkKindRevalidate],
		},
		ElapsedMS:        elapsed.Milliseconds(),
		EnqueueElapsedMS: enqueueElapsed.Milliseconds(),
		BudgetMS:         int64(budgetMS),
		BudgetMet:        budgetMet,
	}

	emitJSON(t, out)

	for _, kind := range []ports.WorkKind{ports.WorkKindEmbed, ports.WorkKindAutoLink, ports.WorkKindRevalidate} {
		if got, want := counts.rows[kind], expectedPerKind; got != want {
			t.Fatalf("rows enqueued for %s = %d, want %d (Promoter contract broken)", kind, got, want)
		}
		if stuck := counts.stuck[kind]; stuck > 0 {
			t.Fatalf("M3 gate-5 FAIL: %d %s rows stuck (pending/in_progress) after budget=%dms",
				stuck, kind, budgetMS)
		}
		// done + failed must cover the enqueued rows. failed rows are
		// allowed (DoD: 'done' OR 'failed' with surfaced reason).
		if counts.done[kind]+counts.failed[kind] != expectedPerKind {
			t.Fatalf("M3 gate-5 FAIL: %s done+failed=%d, want %d",
				kind, counts.done[kind]+counts.failed[kind], expectedPerKind)
		}
	}
	if !budgetMet {
		t.Fatalf("M3 gate-5 FAIL: drain did not complete within budget=%dms (elapsed=%dms)",
			budgetMS, elapsed.Milliseconds())
	}
}

// drivePromotions stages N synthetic single-node files and Promotes each.
// One file per promotion → one row per (file, work_kind) in the queue.
func drivePromotions(ctx context.Context, t *testing.T, staging *application.StagingArea, promoter *application.Promoter, actor domain.Actor, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		filePath := fmt.Sprintf("pkg/fuzz/f%05d.go", i)
		nodeID := fmt.Sprintf("n-fuzz-%05d", i)
		symbol := fmt.Sprintf("pkg/fuzz.Fn%05d", i)
		node, err := domain.NewNode(nodeID, filePath, symbol, domain.KindFunction)
		if err != nil {
			t.Fatalf("domain.NewNode: %v", err)
		}
		staging.StageFile(repoID, branch, filePath, []*domain.Node{node}, nil)

		gitSHA := fmt.Sprintf("sha-%05d", i)
		if err := promoter.Promote(ctx, repoID, branch, gitSHA, actor); err != nil {
			t.Fatalf("promoter.Promote(%d): %v", i, err)
		}
	}
}

// waitDrain polls until every row is terminal (done or failed) or the deadline
// passes. Returns true iff the drain finished before the deadline.
func waitDrain(t *testing.T, db *sql.DB, deadline time.Time) bool {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		var pending int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM post_promotion_queue WHERE state IN ('pending','in_progress')`,
		).Scan(&pending); err != nil {
			t.Fatalf("count non-terminal rows: %v", err)
		}
		if pending == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		<-tick.C
	}
}

type counts struct {
	rows   map[ports.WorkKind]int
	done   map[ports.WorkKind]int
	failed map[ports.WorkKind]int
	stuck  map[ports.WorkKind]int
}

func tallyCounts(t *testing.T, db *sql.DB) counts {
	t.Helper()
	c := counts{
		rows:   map[ports.WorkKind]int{},
		done:   map[ports.WorkKind]int{},
		failed: map[ports.WorkKind]int{},
		stuck:  map[ports.WorkKind]int{},
	}
	rows, err := db.Query(
		`SELECT work_kind, state, COUNT(*) FROM post_promotion_queue GROUP BY work_kind, state`,
	)
	if err != nil {
		t.Fatalf("group counts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var wk, state string
		var n int
		if err := rows.Scan(&wk, &state, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		k := ports.WorkKind(wk)
		c.rows[k] += n
		switch state {
		case "done":
			c.done[k] += n
		case "failed":
			c.failed[k] += n
		case "pending", "in_progress":
			c.stuck[k] += n
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return c
}

// stubHandler is a thread-safe WorkHandler that succeeds for every kind.
// Optional artificial latency (random 0..latencyMS) lets the test exercise
// real concurrency across the per-kind goroutines without depending on real
// embedding / linking / revalidate stacks.
type stubHandler struct {
	latencyMS int
	mu        sync.Mutex
	rng       *rand.Rand
}

func newStubHandler(latencyMS int) *stubHandler {
	return &stubHandler{
		latencyMS: latencyMS,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *stubHandler) Handle(ctx context.Context, _ ports.WorkRow) error {
	if h.latencyMS > 0 {
		h.mu.Lock()
		d := time.Duration(h.rng.Intn(h.latencyMS+1)) * time.Millisecond
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

type result struct {
	Promotions       int            `json:"promotions"`
	RowsPerKind      map[string]int `json:"rows_per_kind"`
	DonePerKind      map[string]int `json:"done_per_kind"`
	FailedPerKind    map[string]int `json:"failed_per_kind"`
	StuckPerKind     map[string]int `json:"stuck_per_kind"`
	ElapsedMS        int64          `json:"elapsed_ms"`
	EnqueueElapsedMS int64          `json:"enqueue_elapsed_ms"`
	BudgetMS         int64          `json:"budget_ms"`
	BudgetMet        bool           `json:"budget_met"`
}

func emitJSON(t *testing.T, r result) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	fmt.Printf("QUEUEFUZZ %s\n", string(b))
	t.Log(string(b))
}

func seedRepo(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
