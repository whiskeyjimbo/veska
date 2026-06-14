package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ExportedSymbolRepo is the SQLite adapter for the ExportedSymbolQuerier port.
// It answers "which EXPORTED public-surface nodes in (repoID, branch) live in a
// set of files?" in a single round-trip, for the breaking-removal diff gate
// (solov2-zvh6.12).
type ExportedSymbolRepo struct {
	db *sql.DB
}

// NewExportedSymbolRepo constructs an ExportedSymbolRepo bound to the given
// read-capable *sql.DB.
func NewExportedSymbolRepo(db *sql.DB) *ExportedSymbolRepo {
	return &ExportedSymbolRepo{db: db}
}

// ExportedSymbolsInFiles returns the exported nodes in (repoID, branch) whose
// file_path is one of filePaths and whose kind is in {function, method,
// interface} — the same public-surface kind set the contract-drift gate judges.
//
// Empty filePaths is a no-op (returns nil, nil) — avoiding a degenerate "IN ()"
// clause that SQLite rejects, symmetric with DriftedNodesInFiles.
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
  AND kind IN ('function','method','interface')
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
