// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/platform/tokenize"
)

// FTSReindexRepo rebuilds the full-text-search rows for a single file. It backs
// the async WorkKindFTS lane: the promote tx no longer writes FTS inserts (only
// ftsSink.BeforeNodeDelete still runs, to clear orphans), so this runs shortly
// after promotion to make the file's symbols searchable again.
type FTSReindexRepo struct {
	writeDB *sql.DB
}

// NewFTSReindexRepo constructs the reindex repo over the write pool.
func NewFTSReindexRepo(writeDB *sql.DB) *FTSReindexRepo {
	return &FTSReindexRepo{writeDB: writeDB}
}

// FTSPendingCounter reports how many files the async FTS lane still owes,
// reading from the read pool. Backs the eng_search_semantic 'fts_pending'
// degraded-reason hint and eng_get_status's pending_fts field.
type FTSPendingCounter struct {
	readDB *sql.DB
}

// NewFTSPendingCounter constructs the counter over the read pool.
func NewFTSPendingCounter(readDB *sql.DB) *FTSPendingCounter {
	return &FTSPendingCounter{readDB: readDB}
}

// CountPendingFTS counts undrained fts queue rows (pending or in-flight).
func (c *FTSPendingCounter) CountPendingFTS(ctx context.Context) (int, error) {
	var n int
	if err := c.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM post_promotion_queue
		   WHERE work_kind = 'fts' AND state IN ('pending','in_progress')`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending fts: %w", err)
	}
	return n, nil
}

// ReindexFile rebuilds node_fts_words + node_fts_trigrams for every node
// currently in (repoID, branch, filePath). It deletes the file's existing FTS
// rows first so a retry (or a re-promote that re-enqueued the file) converges
// to the current node set rather than duplicating - the handler is
// level-triggered, reading whatever nodes exist now, so a stale or duplicate
// queue row is harmless.
//
// The FTS payload mirrors the former synchronous sink exactly - rawFTS is
// `kind + " " + symbol_path` (symbol_path is the column that holds the node's
// name) run through tokenize.Symbol - so search ranking is unchanged.
func (r *FTSReindexRepo) ReindexFile(ctx context.Context, repoID, branch, filePath string) error {
	type row struct {
		nodeID string
		raw    string
	}
	// Read the file's nodes outside the write tx to keep the transaction short.
	rows, err := r.writeDB.QueryContext(ctx,
		`SELECT node_id, kind, symbol_path FROM nodes
		   WHERE repo_id = ? AND branch = ? AND file_path = ?`,
		repoID, branch, filePath,
	)
	if err != nil {
		return fmt.Errorf("fts reindex: load nodes for %q: %w", filePath, err)
	}
	var pending []row
	for rows.Next() {
		var nodeID, kind, symbolPath string
		if err := rows.Scan(&nodeID, &kind, &symbolPath); err != nil {
			_ = rows.Close()
			return fmt.Errorf("fts reindex: scan node for %q: %w", filePath, err)
		}
		pending = append(pending, row{nodeID: nodeID, raw: kind + " " + symbolPath})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("fts reindex: iterate nodes for %q: %w", filePath, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("fts reindex: close nodes cursor for %q: %w", filePath, err)
	}

	tx, err := r.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fts reindex: begin tx for %q: %w", filePath, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Clear this file's current FTS rows so a retry doesn't double-insert.
	// (BeforeNodeDelete already removed the pre-promotion rows synchronously;
	// this covers rows a previous attempt of THIS handler may have written.)
	for _, t := range []string{"node_fts_words", "node_fts_trigrams"} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+t+`
			 WHERE branch = ? AND repo_id = ?
			   AND node_id IN (SELECT node_id FROM nodes
			                   WHERE file_path = ? AND branch = ? AND repo_id = ?)`,
			branch, repoID, filePath, branch, repoID,
		); err != nil {
			return fmt.Errorf("fts reindex: clear %s for %q: %w", t, filePath, err)
		}
	}

	insWords, err := tx.PrepareContext(ctx,
		`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("fts reindex: prepare words insert: %w", err)
	}
	defer insWords.Close()
	insTrigrams, err := tx.PrepareContext(ctx,
		`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("fts reindex: prepare trigrams insert: %w", err)
	}
	defer insTrigrams.Close()

	for _, p := range pending {
		if _, err := insWords.ExecContext(ctx, p.nodeID, branch, repoID, tokenize.Symbol(p.raw)); err != nil {
			return fmt.Errorf("fts reindex: insert words for %q: %w", p.nodeID, err)
		}
		if _, err := insTrigrams.ExecContext(ctx, p.nodeID, branch, repoID, p.raw); err != nil {
			return fmt.Errorf("fts reindex: insert trigrams for %q: %w", p.nodeID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fts reindex: commit for %q: %w", filePath, err)
	}
	return nil
}
