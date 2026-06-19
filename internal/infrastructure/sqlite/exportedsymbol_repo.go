// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ExportedSymbolRepo implements the ExportedSymbolQuerier port. It queries
// exported public-surface nodes within specific files for the breaking-removal
// diff gate.
type ExportedSymbolRepo struct {
	db *sql.DB
}

// NewExportedSymbolRepo constructs an ExportedSymbolRepo.
func NewExportedSymbolRepo(db *sql.DB) *ExportedSymbolRepo {
	return &ExportedSymbolRepo{db: db}
}

// ExportedSymbolsInFiles returns exported nodes in the specified files. It
// includes a wider set of public kinds (including types, structs, and variables)
// than the contract-drift check because removal detection only requires name
// presence. An empty slice of file paths returns early to avoid generating
// an invalid SQL IN clause.
func (r *ExportedSymbolRepo) ExportedSymbolsInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ports.ExportedSymbol, error) {
	if len(filePaths) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(filePaths))
	args := make([]any, 0, len(filePaths)+2)
	args = append(args, repoID, branch)
	for i, p := range filePaths {
		placeholders[i] = "?"
		args = append(args, p)
	}

	query := fmt.Sprintf(`
SELECT node_id, file_path, kind, symbol_path
FROM nodes
WHERE repo_id = ?
  AND branch = ?
  AND file_path IN (%s)
  AND kind IN ('function','method','interface','struct','type','variable','class')
  AND COALESCE(exported, 0) = 1
ORDER BY file_path, node_id`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ExportedSymbolRepo.ExportedSymbolsInFiles: %w", err)
	}
	defer rows.Close()

	var out []ports.ExportedSymbol
	for rows.Next() {
		var s ports.ExportedSymbol
		if err := rows.Scan(&s.NodeID, &s.FilePath, &s.Kind, &s.Name); err != nil {
			return nil, fmt.Errorf("sqlite.ExportedSymbolRepo.ExportedSymbolsInFiles: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.ExportedSymbolRepo.ExportedSymbolsInFiles: rows: %w", err)
	}
	return out, nil
}
