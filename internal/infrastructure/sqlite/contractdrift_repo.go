package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ContractDriftRepo is the SQLite adapter for the ContractDriftQuerier port.
// It answers "which nodes in (repoID, branch) whose file_path is in a set have
// a signature that differs from prev_signature?" in a single round-trip.
type ContractDriftRepo struct {
	db *sql.DB
}

// NewContractDriftRepo constructs a ContractDriftRepo bound to the given
// read-capable *sql.DB. The handle must point at a DB with migration 0005
// applied (nodes table has signature + prev_signature columns).
func NewContractDriftRepo(db *sql.DB) *ContractDriftRepo {
	return &ContractDriftRepo{db: db}
}

// DriftedNodesInFiles returns nodes in (repoID, branch) whose file_path is one
// of filePaths, whose prev_signature and signature are both non-NULL, whose
// kind is in {function, method, interface}, and whose prev_signature differs
// from signature.
//
// Empty filePaths is a no-op (returns nil, nil) — this avoids building a
// degenerate "IN ()" clause that SQLite rejects.
//
// The query intentionally applies the kind filter at the storage layer (it
// uses a closed enum the index can evaluate cheaply) but does not encode any
// severity / message / anchor policy — those live in the application-layer
// ContractDriftCheck.
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
