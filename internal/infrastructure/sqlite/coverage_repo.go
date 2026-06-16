package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CoverageRepo is the SQLite adapter for the CoverageQuerier port. It answers
// "for each node in (repoID, branch) whose file_path is in a set, what are the
// distinct file paths of its DIRECT inbound CALLS callers?" — the read-side
// attribution the untested-symbol check consumes.
type CoverageRepo struct {
	db *sql.DB
}

// NewCoverageRepo constructs a CoverageRepo bound to the given read-capable
// *sql.DB. The handle must point at a DB with migration 0001 applied (nodes +
// edges tables).
func NewCoverageRepo(db *sql.DB) *CoverageRepo {
	return &CoverageRepo{db: db}
}

// CandidateCallersInFiles returns every node in (repoID, branch) whose
// file_path is one of filePaths, each paired with the distinct file paths of
// its direct inbound CALLS callers. A candidate with no caller appears with an
// empty CallerFiles slice.
// Liveness is attributed against CALLS edges ONLY (case-insensitive), matching
// the dead-code adapter: a CONTAINS edge from the parent package or a
// SIMILAR_TO autolink edge is not a "caller". The LEFT JOINs keep every
// candidate node even when it has zero callers; a CALLS edge whose source node
// is missing contributes a NULL caller file, which is skipped.
// No name/kind/test-file filtering is applied here — those rules live in the
// application-layer check so they stay trivially testable without a database.
// Empty filePaths is a no-op (nil, nil), avoiding a degenerate "IN " clause.
func (r *CoverageRepo) CandidateCallersInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ports.NodeCallers, error) {
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
SELECT n.node_id, n.file_path, n.kind, n.symbol_path,
       COALESCE(n.line_start, 0), COALESCE(n.line_end, 0),
       COALESCE(n.content_hash, ''), src.file_path AS caller_file
FROM nodes n
LEFT JOIN edges e
  ON e.dst_node_id = n.node_id AND e.branch = n.branch AND UPPER(e.kind) = 'CALLS'
LEFT JOIN nodes src
  ON src.node_id = e.src_node_id AND src.branch = n.branch
WHERE n.repo_id = ?
  AND n.branch = ?
  AND n.file_path IN (%s)
ORDER BY n.file_path, n.node_id`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.CoverageRepo.CandidateCallersInFiles: %w", err)
	}
	defer rows.Close()

	// Accumulate caller files per node, preserving first-seen candidate order
	// and de-duplicating caller file paths.
	order := make([]string, 0)
	refByID := make(map[string]ports.NodeRef)
	callersByID := make(map[string][]string)
	seen := make(map[string]map[string]struct{})

	for rows.Next() {
		var ref ports.NodeRef
		var callerFile sql.NullString
		if err := rows.Scan(&ref.NodeID, &ref.FilePath, &ref.Kind, &ref.Name,
			&ref.LineStart, &ref.LineEnd, &ref.ContentHash, &callerFile); err != nil {
			return nil, fmt.Errorf("sqlite.CoverageRepo.CandidateCallersInFiles: scan: %w", err)
		}
		if _, ok := refByID[ref.NodeID]; !ok {
			refByID[ref.NodeID] = ref
			seen[ref.NodeID] = make(map[string]struct{})
			order = append(order, ref.NodeID)
		}
		if callerFile.Valid && callerFile.String != "" {
			if _, dup := seen[ref.NodeID][callerFile.String]; !dup {
				seen[ref.NodeID][callerFile.String] = struct{}{}
				callersByID[ref.NodeID] = append(callersByID[ref.NodeID], callerFile.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.CoverageRepo.CandidateCallersInFiles: rows: %w", err)
	}

	out := make([]ports.NodeCallers, 0, len(order))
	for _, id := range order {
		out = append(out, ports.NodeCallers{Node: refByID[id], CallerFiles: callersByID[id]})
	}
	return out, nil
}

// InboundCallsEdges returns, for each node_id in nodeIDs, the source nodes of
// its inbound CALLS edges in (repoID, branch), hydrated with the metadata the
// reverse-map BFS needs to classify test entrypoints (kind, symbol_path, file).
// It is the CALLS-scoped, metadata-bearing adjacency primitive behind the
// node→test reverse map — the transitive sibling of
// CandidateCallersInFiles, which is direct-only and file-granularity.
// CALLS edges ONLY (case-insensitive), matching the coverage proxy: a CONTAINS
// or SIMILAR_TO edge is not a caller. Every queried id is present in the result
// map (empty slice when it has no inbound CALLS caller), so the BFS can rely on
// a present key meaning "queried". Empty nodeIDs is a no-op (empty map).
func (r *CoverageRepo) InboundCallsEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]ports.NodeRef, error) {
	out := make(map[string][]ports.NodeRef, len(nodeIDs))
	for _, id := range nodeIDs {
		out[id] = nil // present-key contract; overwritten below if it has callers
	}
	if len(nodeIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+2)
	args = append(args, repoID, branch)
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := fmt.Sprintf(`
SELECT e.dst_node_id AS queried,
       src.node_id, src.file_path, src.kind, src.symbol_path,
       COALESCE(src.line_start, 0), COALESCE(src.line_end, 0),
       COALESCE(src.content_hash, '')
FROM edges e
JOIN nodes src
  ON src.node_id = e.src_node_id AND src.branch = e.branch
WHERE e.repo_id = ?
  AND e.branch = ?
  AND UPPER(e.kind) = 'CALLS'
  AND e.dst_node_id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.CoverageRepo.InboundCallsEdges: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var queried string
		var ref ports.NodeRef
		if err := rows.Scan(&queried, &ref.NodeID, &ref.FilePath, &ref.Kind, &ref.Name,
			&ref.LineStart, &ref.LineEnd, &ref.ContentHash); err != nil {
			return nil, fmt.Errorf("sqlite.CoverageRepo.InboundCallsEdges: scan: %w", err)
		}
		out[queried] = append(out[queried], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.CoverageRepo.InboundCallsEdges: rows: %w", err)
	}
	return out, nil
}
