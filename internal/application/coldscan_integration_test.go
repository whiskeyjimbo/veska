package application_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	infrafs "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// fakeColdScanGit supplies a fixed HEAD; the cold-scan reparser only consults
// GitQuerier.HEAD — the remaining methods are unused on the full-reparse path.
type fakeColdScanGit struct{ head string }

func (f *fakeColdScanGit) HEAD(string) (string, error)                     { return f.head, nil }
func (f *fakeColdScanGit) IsAncestor(string, string, string) (bool, error) { return false, nil }
func (f *fakeColdScanGit) CommitsSince(string, string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeColdScanGit) ChangedFiles(string, string) ([]string, error) { return nil, nil }
func (f *fakeColdScanGit) ReadFileAtCommit(string, string, string) ([]byte, error) {
	return nil, nil
}

// realIgnoreLoader adapts the production .veskaignore loader to the
// application-layer IgnoreLoader contract. Defined locally so the external
// test package does not depend on the whitebox test helpers.
func realIgnoreLoader(repoRoot string) (application.IgnoreMatcher, error) {
	return infrafs.Load(repoRoot)
}

// writeIntFile drops a fixture file under dir, creating parent dirs as needed.
func writeIntFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

// TestColdScanReparser_Integration_RealPipeline runs the cold-scan reparser
// over a fixture tree against a real tree-sitter parser, a real Ingester, and
// a real Promoter backed by sqlite.PromotionStore (FTS + EmbedRef sinks). It
// asserts (1) nodes from the fixture files land in the promoted graph, and
// (2) running the reparser a second time leaves the node count unchanged
// (pipeline-level idempotency).
//
// Scope: base sqlite only — the vector backend is downstream of the embedder
// worker (tracked separately as solov2-asn).
func TestColdScanReparser_Integration_RealPipeline(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	// Fixture: two Go files in a temp dir.
	tmp := t.TempDir()
	writeIntFile(t, tmp, "a.go", "package fixture\n\nfunc Alpha() int { return 1 }\n")
	writeIntFile(t, tmp, "b.go", "package fixture\n\nfunc Beta() string { return \"b\" }\n")

	// Real pipeline: tree-sitter GoParser → Ingester → Promoter (sqlite store).
	parser := treesitter.NewGoParser()
	staging := application.NewStagingArea()
	gate := application.NewIngestionGate(staging)
	ingester := application.NewIngester(parser, staging, gate)
	promoter := newTestPromoter(staging, db)

	reparser, err := application.NewColdScanReparser(
		ingester, promoter, &fakeColdScanGit{head: "sha-1"},
		application.WithIgnoreLoader(realIgnoreLoader),
	)
	if err != nil {
		t.Fatalf("NewColdScanReparser: %v", err)
	}

	rec := application.RepoRecord{
		RepoID:       "repo1",
		RootPath:     tmp,
		ActiveBranch: "main",
	}

	// First run: should promote at least one node per fixture file.
	if err := reparser(context.Background(), rec); err != nil {
		t.Fatalf("reparser run 1: %v", err)
	}
	after1 := countNodes(t, db)
	if after1 < 2 {
		t.Fatalf("nodes after run 1: want >= 2 (one per fixture file), got %d", after1)
	}

	// Second run on the unchanged tree must leave the promoted graph stable:
	// the DELETE+INSERT promotion path collapses repeat content to the same
	// row set, so the count must not grow.
	if err := reparser(context.Background(), rec); err != nil {
		t.Fatalf("reparser run 2: %v", err)
	}
	after2 := countNodes(t, db)
	if after2 != after1 {
		t.Errorf("idempotency: nodes after run 2 = %d, want %d (same as run 1)", after2, after1)
	}

	// EmbedRefSink should have enqueued one ref per node (state=pending).
	if got := countEmbedRefs(t, db); got != after2 {
		t.Errorf("node_embedding_refs: want %d (one per node), got %d", after2, got)
	}

	// Promotion-queue rows must also be present (FTS + embedding work_kinds).
	if got := countQueue(t, db); got == 0 {
		t.Error("post_promotion_queue: want > 0, got 0 — promotion sinks did not run")
	}

	// Sanity: a node row carries a non-empty symbol_path drawn from the parser.
	var sample string
	if err := db.QueryRow(`SELECT symbol_path FROM nodes LIMIT 1`).Scan(&sample); err != nil {
		t.Fatalf("query sample node: %v", err)
	}
	if sample == "" {
		t.Error("sample node symbol_path empty — parser likely produced no useful nodes")
	}
}

func countEmbedRefs(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs`).Scan(&n); err != nil {
		t.Fatalf("countEmbedRefs: %v", err)
	}
	return n
}
