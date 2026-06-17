// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"database/sql"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func openArchiveTestDB(t *testing.T) (*sql.DB, *sqlite.EmbeddingArchive) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, sqlite.NewEmbeddingArchive(db, db)
}

// seedReady seeds a repository, a node, and a ready embedding reference for loading tests.
func seedReady(t *testing.T, db *sql.DB, repoID, branch, nodeID, contentHash, model string, blob []byte, dim int) {
	t.Helper()
	now := time.Now().UnixMilli()
	mustExec(t, db, `INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now)
	mustExec(t, db, `INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, branch, repoID, "go", "function", nodeID, "f.go", contentHash, now, "test", "system")
	mustExec(t, db, `INSERT OR IGNORE INTO node_embeddings (content_hash, model, dim, embedding, created_at)
		VALUES (?,?,?,?,?)`, contentHash, model, dim, blob, now)
	mustExec(t, db, `INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at, attempts)
		VALUES (?,?, 'ready', ?, ?, 1)`, nodeID, contentHash, now, now)
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func TestEmbeddingArchive_LoadReadyEmbeddings(t *testing.T) {
	t.Parallel()
	db, archive := openArchiveTestDB(t)

	seedReady(t, db, "repo1", "main", "n1", "h1", "m1", []byte{1, 2, 3}, 3)
	seedReady(t, db, "repo1", "topic", "n2", "h2", "m1", []byte{4, 5, 6}, 3)

	now := time.Now().UnixMilli()
	mustExec(t, db, `INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES ('n3','main','repo1','go','function','n3','f.go','h3',?, 'test','system')`, now)
	mustExec(t, db, `INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at)
		VALUES ('n3', NULL, 'pending', ?)`, now)

	rows, err := archive.LoadReadyEmbeddings(context.Background())
	if err != nil {
		t.Fatalf("LoadReadyEmbeddings: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (pending excluded); %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.NodeID == "n3" {
			t.Errorf("pending node n3 was returned")
		}
		if r.Dim != 3 || len(r.Blob) != 3 {
			t.Errorf("row %s: dim=%d blob=%d, want 3/3", r.NodeID, r.Dim, len(r.Blob))
		}
	}
}

func TestEmbeddingArchive_RequeueAllUnderNewModel(t *testing.T) {
	t.Parallel()
	db, archive := openArchiveTestDB(t)

	seedReady(t, db, "repo1", "main", "n1", "h1", "old-model", []byte{0, 0, 0}, 3)
	seedReady(t, db, "repo1", "main", "n2", "h2", "old-model", []byte{0, 0, 0}, 3)

	n, err := archive.RequeueAllUnderNewModel(context.Background())
	if err != nil {
		t.Fatalf("RequeueAllUnderNewModel: %v", err)
	}
	if n != 2 {
		t.Errorf("rows requeued = %d, want 2", n)
	}

	var embCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&embCount); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embCount != 0 {
		t.Errorf("node_embeddings not cleared: %d rows remain", embCount)
	}

	rows, err := db.Query(`SELECT node_id, content_hash, state, embedded_at, attempts FROM node_embedding_refs ORDER BY node_id`)
	if err != nil {
		t.Fatalf("query refs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, state string
		var ch sql.NullString
		var emb sql.NullInt64
		var attempts int
		if err := rows.Scan(&nodeID, &ch, &state, &emb, &attempts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if state != "pending" || ch.Valid || emb.Valid || attempts != 0 {
			t.Errorf("ref %s: state=%q content_hash=%v embedded_at=%v attempts=%d, want pending/NULL/NULL/0",
				nodeID, state, ch, emb, attempts)
		}
	}
}
