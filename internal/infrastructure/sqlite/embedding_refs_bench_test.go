// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// BenchmarkApplyEmbedBatch measures the per-batch write path the cold-scan embed
// drain is bound by: one transaction that inserts B unique embeddings and flips
// B refs to ready. Isolated from embed compute and read classify. NOTE: wall-time
// here is fsync-bound (one commit per batch) and varies widely run-to-run; the
// stable signal is allocs/op. The lever for fewer fsyncs is rows-per-commit
// (batch size), not statements-per-commit.
// Run: go test -tags sqlite_fts5 -bench ApplyEmbedBatch -benchmem ./internal/infrastructure/sqlite/
func BenchmarkApplyEmbedBatch(b *testing.B) {
	const batch = 128
	dbPath := filepath.Join(b.TempDir(), "v.db")
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(b.TempDir(), "bk")}); err != nil {
		b.Fatalf("OpenWithOptions: %v", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		b.Fatalf("OpenPools: %v", err)
	}
	b.Cleanup(func() { _ = pools.Close() })

	now := time.Now().UnixMilli()
	if _, err := pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"bench", "/tmp/bench", now,
	); err != nil {
		b.Fatalf("seed repo: %v", err)
	}
	// Seed the batch's refs as pending so the ready-flip UPDATE matches real rows
	// (node rows are unnecessary: node_embedding_refs has no FK to nodes).
	for j := range batch {
		if _, err := pools.Write.Exec(
			`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
			fmt.Sprintf("n%d", j), now,
		); err != nil {
			b.Fatalf("seed ref %d: %v", j, err)
		}
	}

	repo := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write)
	ctx := context.Background()
	at := time.Now()
	embedding := make([]byte, 256*4) // ~realistic vector blob

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		inserts := make([]ports.EmbedInsert, batch)
		ready := make([]ports.EmbedReadyRef, batch)
		for j := range batch {
			h := fmt.Sprintf("h-%d-%d", i, j) // unique so every row really inserts
			inserts[j] = ports.EmbedInsert{ContentHash: h, Dim: 256, Embedding: embedding}
			ready[j] = ports.EmbedReadyRef{NodeID: fmt.Sprintf("n%d", j), ContentHash: h}
		}
		if err := repo.ApplyEmbedBatch(ctx, inserts, ready, nil, "bench-model", 3, at); err != nil {
			b.Fatalf("ApplyEmbedBatch: %v", err)
		}
	}
}
