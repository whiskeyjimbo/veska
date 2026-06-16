//go:build eval

// Package embedder drives the M3 gate-1 embedder throughput bench.
// Goal: drive the real embedder.Worker against a real local Ollama instance
// for a measurement window and assert sustained throughput is at or above the
// gate-1 floor (5 emb/s, see M3.md exit gates).
// What this measures: the WORKER's sustained output rate — i.e. the rate that
// production observes, which is the min of (Ollama capacity, Worker rate
// limiter). The Worker defaults to 10 emb/s; the gate floor is 5 emb/s; if
// Ollama is healthy the run reports a rate close to the limiter cap. A rate
// significantly below the limiter cap points at Ollama, not the worker.
// Build-tag-gated; the make target is `make eval-embed-throughput`. The test
// skips with a clear message if Ollama is not reachable.
package embedder

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
)

const (
	defaultDurationS = 60
	defaultSeedN     = 2000
	defaultGateMin   = 5.0
	defaultOllamaURL = "http://localhost:11434"
	defaultModel     = "nomic-embed-text"

	repoID = "embed-bench-eval"
	branch = "main"
)

// result is the JSON output payload — keys are pinned by the bead spec.
type result struct {
	Model           string  `json:"model"`
	OllamaURL       string  `json:"ollama_url"`
	SeedN           int     `json:"seed_n"`
	DurationS       int     `json:"duration_s"`
	EmbedsCompleted int     `json:"embeds_completed"`
	RatePerSec      float64 `json:"rate_per_sec"`
	GateMinRate     float64 `json:"gate_min_rate"`
	GateMet         bool    `json:"gate_met"`
}

// TestEmbedderThroughput is the M3 gate-1 throughput bench.
func TestEmbedderThroughput(t *testing.T) {
	durationS := envInt("EMBED_BENCH_DURATION_S", defaultDurationS)
	seedN := envInt("EMBED_BENCH_SEED_N", defaultSeedN)
	gateMin := envFloat("EMBED_BENCH_GATE_MIN_RATE", defaultGateMin)
	ollamaURL := envStr("VESKA_OLLAMA_URL", defaultOllamaURL)
	model := envStr("VESKA_EMBED_MODEL", defaultModel)

	if durationS <= 0 {
		t.Fatalf("EMBED_BENCH_DURATION_S must be > 0, got %d", durationS)
	}
	if seedN <= 0 {
		t.Fatalf("EMBED_BENCH_SEED_N must be > 0, got %d", seedN)
	}

	// Sanity: keep the worker fed at the upper end of the target range.
	// 20 emb/s * durationS embeds consumed in the window; seed should
	// exceed that with headroom so CountPending never bottoms out.
	if minSeed := durationS * 25; seedN < minSeed {
		t.Logf("warning: seed_n=%d may be too low for duration=%ds (recommend >= %d)",
			seedN, durationS, minSeed)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	if err := probeOllama(probeCtx, ollamaURL); err != nil {
		t.Skipf("Ollama not reachable at %s (%v) — skipping M3 gate-1 throughput bench", ollamaURL, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

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

	seedPending(t, pools.Write, seedN)

	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write)
	provider, err := ollama.New(model, ollama.WithBaseURL(ollamaURL))
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}
	vectors := memvec.New()

	// Use defaults: 10 emb/s limiter, 32 batch, 250ms interval. The point
	// of gate-1 is to measure the *worker's* sustained output, not to
	// uncap Ollama.
	worker, err := embedder.NewWorker(refs, provider, vectors)
	if err != nil {
		t.Fatalf("embedder.NewWorker: %v", err)
	}

	// Sanity-check starting depth so a mis-seed surfaces before the run.
	startingPending, err := refs.CountPending(ctx)
	if err != nil {
		t.Fatalf("CountPending (pre-start): %v", err)
	}
	if startingPending != seedN {
		t.Fatalf("seed mismatch: CountPending=%d, want %d", startingPending, seedN)
	}

	started := time.Now()
	worker.Start(ctx)
	t.Cleanup(func() {
		worker.Stop()
		worker.Wait()
	})

	// Wait the measurement window, honouring ctx cancellation. The window
	// is the explicit gate input — we don't end early on drain because
	// "rate" is defined over a fixed wall-clock interval.
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before measurement window elapsed: %v", ctx.Err())
	case <-time.After(time.Duration(durationS) * time.Second):
	}

	pendingNow, err := refs.CountPending(ctx)
	if err != nil {
		t.Fatalf("CountPending (post-window): %v", err)
	}

	// Stop synchronously so any in-flight Embed call returns before we
	// finalise the count. The window is the input; CountPending is read
	// AFTER Stop to keep the embeds_completed value internally consistent
	// with the duration.
	worker.Stop()
	worker.Wait()

	// Re-read after Stop in case a tick was mid-flight when the window expired.
	finalPending, err := refs.CountPending(ctx)
	if err != nil {
		t.Fatalf("CountPending (post-stop): %v", err)
	}
	// Prefer the larger drain (i.e. smaller pending) — but the canonical
	// measurement is the post-window read because Stop adds latency outside
	// the measured interval. Log both so a reviewer can see the in-flight
	// delta.
	if finalPending < pendingNow {
		t.Logf("post-stop drained %d additional refs after the window (in-flight at cancel)",
			pendingNow-finalPending)
	}

	duration := time.Duration(durationS) * time.Second
	completed := seedN - pendingNow
	rate := float64(completed) / duration.Seconds()
	elapsed := time.Since(started)

	out := result{
		Model:           model,
		OllamaURL:       ollamaURL,
		SeedN:           seedN,
		DurationS:       durationS,
		EmbedsCompleted: completed,
		RatePerSec:      rate,
		GateMinRate:     gateMin,
		GateMet:         rate >= gateMin,
	}
	emitJSON(t, out)
	t.Logf("started→post-window elapsed=%s (window=%s)", elapsed, duration)

	if completed < 0 {
		t.Fatalf("embeds_completed=%d is negative — seed or count is wrong", completed)
	}
	if !out.GateMet {
		t.Fatalf("M3 gate-1 FAIL: rate=%.2f emb/s < floor=%.2f emb/s (completed=%d in %ds)",
			rate, gateMin, completed, durationS)
	}
}

// probeOllama issues a quick GET /api/tags. Any non-2xx response or transport
// failure is reported as an error so the caller can t.Skip cleanly.
func probeOllama(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// seedPending inserts one repo row, n nodes, and n pending embedding refs in a
// single transaction. The text projection used by the Worker is
// "<kind> <symbol_path>"; varying the symbol_path per row keeps dedup off the
// critical path so each ref drives a real Embed call.
func seedPending(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	now := time.Now().UnixMilli()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("seed: begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now,
	); err != nil {
		t.Fatalf("seed: insert repo: %v", err)
	}

	nodeStmt, err := tx.Prepare(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("seed: prepare nodes: %v", err)
	}
	defer nodeStmt.Close()

	refStmt, err := tx.Prepare(
		`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
	)
	if err != nil {
		t.Fatalf("seed: prepare refs: %v", err)
	}
	defer refStmt.Close()

	for i := 0; i < n; i++ {
		nodeID := fmt.Sprintf("embed-bench-n%07d", i)
		symbol := fmt.Sprintf("pkg/embedbench.Fn%07d", i)
		filePath := fmt.Sprintf("pkg/embedbench/f%07d.go", i)
		contentHash := fmt.Sprintf("ch-%07d", i)
		if _, err := nodeStmt.Exec(
			nodeID, branch, repoID, "go", "function", symbol, filePath,
			contentHash, now, "embed-bench", "system",
		); err != nil {
			t.Fatalf("seed: insert node %d: %v", i, err)
		}
		if _, err := refStmt.Exec(nodeID, now); err != nil {
			t.Fatalf("seed: insert ref %d: %v", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("seed: commit: %v", err)
	}
}

func emitJSON(t *testing.T, r result) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	fmt.Printf("EMBED_BENCH %s\n", string(b))
	t.Log(string(b))
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
