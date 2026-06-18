// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/summary"
)

// SummaryStore is the SQLite adapter for summary.Store. Reads run on the read
// pool; the single short_summary UPDATE runs on the serialised write pool.
type SummaryStore struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

var _ summary.Store = (*SummaryStore)(nil)

// NewSummaryStore constructs a SummaryStore over the read/write pools.
func NewSummaryStore(readDB, writeDB *sql.DB) *SummaryStore {
	return &SummaryStore{readDB: readDB, writeDB: writeDB}
}

// PromotedNodes returns the nodes for filePath on repoID/branch with the fields
// the summary lane needs. Kind filtering is left to the caller so the lane owns
// the container-exclusion policy.
func (s *SummaryStore) PromotedNodes(ctx context.Context, repoID, branch, filePath string) ([]summary.Node, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT node_id, kind, symbol_path, signature, line_start, line_end
		   FROM nodes
		  WHERE repo_id = ? AND branch = ? AND file_path = ?`,
		repoID, branch, filePath)
	if err != nil {
		return nil, fmt.Errorf("summary store: query nodes for %q: %w", filePath, err)
	}
	defer rows.Close()

	var out []summary.Node
	for rows.Next() {
		var (
			n          summary.Node
			symbolPath string
			signature  sql.NullString
			lineStart  sql.NullInt64
			lineEnd    sql.NullInt64
		)
		if err := rows.Scan(&n.NodeID, &n.Kind, &symbolPath, &signature, &lineStart, &lineEnd); err != nil {
			return nil, fmt.Errorf("summary store: scan node for %q: %w", filePath, err)
		}
		n.Name = symbolPath
		if signature.Valid {
			n.Signature = signature.String
		}
		n.LineStart = int(lineStart.Int64)
		n.LineEnd = int(lineEnd.Int64)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summary store: iterate nodes for %q: %w", filePath, err)
	}
	return out, nil
}

// SetShortSummary persists summary for one node. A row removed by a concurrent
// reparse simply updates zero rows, which is not an error.
func (s *SummaryStore) SetShortSummary(ctx context.Context, repoID, branch, nodeID, summaryText string) error {
	if _, err := s.writeDB.ExecContext(ctx,
		`UPDATE nodes SET short_summary = ? WHERE node_id = ? AND branch = ? AND repo_id = ?`,
		summaryText, nodeID, branch, repoID); err != nil {
		return fmt.Errorf("summary store: set summary for %q: %w", nodeID, err)
	}
	return nil
}
