package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/tokenize"
)

// seedLexical seeds FTS indexes with words and trigrams using the Promoter
// write contract.
func seedLexical(t *testing.T, db *sql.DB, nodeID, branch, repoID, kind, symbolPath string) {
	t.Helper()
	raw := kind + " " + symbolPath
	words := tokenize.Symbol(raw)
	if _, err := db.Exec(
		`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`,
		nodeID, branch, repoID, words,
	); err != nil {
		t.Fatalf("seed words: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`,
		nodeID, branch, repoID, raw,
	); err != nil {
		t.Fatalf("seed trigrams: %v", err)
	}
}

func openLexDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLexical_PrefixHitOnWordsArm(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	seedLexical(t, db, "n1", "main", "r1", "function", "pkg/api/closeFinding")

	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "close", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "n1" {
		t.Fatalf("expected n1, got %+v", hits)
	}
}

func TestLexical_CamelCaseSplit(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	seedLexical(t, db, "n1", "main", "r1", "function", "pkg/api/closeFinding")

	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "Find", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.NodeID == "n1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected n1 in results for query 'Find', got %+v", hits)
	}
}

func TestLexical_TrigramTypoTolerance(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	seedLexical(t, db, "n1", "main", "r1", "function", "pkg/api/closeFinding")

	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "closeFnd", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.NodeID == "n1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected n1 via trigram substring for 'closeFnd', got %+v", hits)
	}
}

func TestLexical_RRFCombinesArms(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	// n1: kicked up by both words ("close") and trigrams (substring "lose").
	seedLexical(t, db, "n1", "main", "r1", "function", "closeFinding")
	// n2: only the trigram arm sees this - words won't match "close".
	seedLexical(t, db, "n2", "main", "r1", "function", "loseTracker")

	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "close", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	// n1 must rank ahead of n2.
	pos := make(map[string]int)
	for i, h := range hits {
		pos[h.NodeID] = i
	}
	if _, ok := pos["n1"]; !ok {
		t.Errorf("n1 missing from results: %+v", hits)
	}
	if ok := pos["n2"] != 0 || (pos["n1"] < pos["n2"]); ok {
		// Acceptable.
	} else if pos["n1"] > pos["n2"] {
		t.Errorf("n1 should rank ahead of n2 (RRF over both arms), got order %+v", hits)
	}
}

func TestLexical_EmptyQuery(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if hits != nil {
		t.Errorf("expected nil for empty query, got %+v", hits)
	}
}

func TestLexical_ScopesToRepoAndBranch(t *testing.T) {
	t.Parallel()
	db := openLexDB(t)
	seedLexical(t, db, "n1", "main", "r1", "function", "closeFinding")
	seedLexical(t, db, "n2", "feature", "r1", "function", "closeFinding")
	seedLexical(t, db, "n3", "main", "r2", "function", "closeFinding")

	repo := sqlite.NewLexicalRepo(db)
	hits, err := repo.Search(context.Background(), "r1", "main", "close", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "n1" {
		t.Errorf("expected only n1 (r1/main), got %+v", hits)
	}
}
