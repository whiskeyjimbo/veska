package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ContractDriftRepo implements ports.ContractDriftQuerier using a SQLite database, querying signature changes across files in a single round-trip.
type ContractDriftRepo struct {
	db *sql.DB
}

// NewContractDriftRepo constructs a ContractDriftRepo bound to the given sql.DB.
func NewContractDriftRepo(db *sql.DB) *ContractDriftRepo {
	return &ContractDriftRepo{db: db}
}

// DriftedNodesInFiles returns nodes in (repoID, branch) whose file_path matches filePaths, whose prev_signature differs from signature, and whose kind is a function, method, or interface.
// An empty slice of filePaths is treated as a no-op to avoid producing a degenerate IN clause.
func (r *ContractDriftRepo) DriftedNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ports.DriftedNode, error) {
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
SELECT node_id, file_path, kind, symbol_path,
       COALESCE(prev_signature, ''),
       COALESCE(signature, ''),
       COALESCE(line_start, 0), COALESCE(line_end, 0),
       COALESCE(content_hash, ''),
       COALESCE(exported, 0)
FROM nodes
WHERE repo_id = ?
  AND branch = ?
  AND file_path IN (%s)
  AND kind IN ('function','method','interface')
  AND prev_signature IS NOT NULL
  AND signature      IS NOT NULL
  AND prev_signature != signature
ORDER BY file_path, node_id`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ContractDriftRepo.DriftedNodesInFiles: %w", err)
	}
	defer rows.Close()

	var out []ports.DriftedNode
	for rows.Next() {
		var d ports.DriftedNode
		if err := rows.Scan(&d.NodeID, &d.FilePath, &d.Kind, &d.Name, &d.PrevSig, &d.NewSig, &d.LineStart, &d.LineEnd, &d.ContentHash, &d.Exported); err != nil {
			return nil, fmt.Errorf("sqlite.ContractDriftRepo.DriftedNodesInFiles: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.ContractDriftRepo.DriftedNodesInFiles: rows: %w", err)
	}
	return out, nil
}
