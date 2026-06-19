// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// escapeGlob wraps GLOB metacharacters (*, ?, [) in character classes because
// GLOB lacks an ESCAPE clause. Since identifier characters generally do not
// include these metacharacters, this function provides defense-in-depth for
// unexpected language inputs.
func escapeGlob(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FindNodes matches fully-qualified symbols exactly or by suffix. We use
// SQLite's GLOB operator instead of LIKE because LIKE is case-insensitive on
// ASCII characters, which would incorrectly conflate distinct symbols. GLOB
// wildcard characters are escaped to prevent interpreting them as character
// classes.
func (r *GraphRepo) FindNodes(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error) {
	suffixPattern := `*.` + escapeGlob(symbolName)
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE repo_id = ? AND branch = ?
		   AND (symbol_path = ? OR symbol_path GLOB ?)
		 ORDER BY (symbol_path = ?) DESC, symbol_path`,
		repoID, branch, symbolName, suffixPattern, symbolName)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: find nodes: %w", err)
	}
	defer rows.Close()

	var out []*domain.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("graph_repo: scan node: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate nodes: %w", err)
	}
	return out, nil
}

// NodesForFile returns all nodes residing in the specified file path.
func (r *GraphRepo) NodesForFile(ctx context.Context, repoID, branch, filePath string) ([]*domain.Node, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE repo_id = ? AND branch = ? AND file_path = ?`,
		repoID, branch, filePath)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: nodes for file %q: %w", filePath, err)
	}
	defer rows.Close()

	var out []*domain.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("graph_repo: scan node for file %q: %w", filePath, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate nodes for file %q: %w", filePath, err)
	}
	return out, nil
}

// GetNode retrieves a single node by ID. A missing node returns (nil, nil)
// without an error.
func (r *GraphRepo) GetNode(ctx context.Context, repoID, branch string, id domain.NodeID) (*domain.Node, error) {
	row := r.readDB.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE repo_id = ? AND branch = ? AND node_id = ?`,
		repoID, branch, string(id))
	n, err := scanNode(row)
	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("graph_repo: get node %q: %w", id, err)
	}
	return n, nil
}

// GetNodeSnippet returns the persisted body snippet for a single node. The
// returned text is capped by the write path and is not guaranteed to cover the
// full source.
func (r *GraphRepo) GetNodeSnippet(ctx context.Context, repoID, branch string, id domain.NodeID) (string, error) {
	var snippet sql.NullString
	err := r.readDB.QueryRowContext(ctx,
		`SELECT snippet FROM nodes WHERE repo_id = ? AND branch = ? AND node_id = ?`,
		repoID, branch, string(id),
	).Scan(&snippet)
	switch {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("graph_repo: get node snippet %q: %w", id, err)
	}
	if !snippet.Valid {
		return "", nil
	}
	return snippet.String, nil
}

// FindNodeByID retrieves the first node matching the content-hashed ID. Because
// node IDs are SHA-256 hashes, collisions across repositories are highly
// unlikely.
func (r *GraphRepo) FindNodeByID(ctx context.Context, id domain.NodeID) (*domain.Node, error) {
	row := r.readDB.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE node_id = ? LIMIT 1`,
		string(id))
	n, err := scanNode(row)
	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("graph_repo: find node %q: %w", id, err)
	}
	return n, nil
}

// FindNodeIDsByPrefix returns distinct node IDs starting with the prefix.
// Because SHA-256 hex node IDs do not contain LIKE wildcard characters, the
// prefix can be safely interpolated without an ESCAPE clause.
func (r *GraphRepo) FindNodeIDsByPrefix(ctx context.Context, prefix string, limit int) ([]domain.NodeID, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT DISTINCT node_id FROM nodes WHERE node_id LIKE ? || '%' LIMIT ?`,
		prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: find node ids by prefix %q: %w", prefix, err)
	}
	defer rows.Close()
	var out []domain.NodeID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("graph_repo: scan node id prefix %q: %w", prefix, err)
		}
		out = append(out, domain.NodeID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate node id prefix %q: %w", prefix, err)
	}
	return out, nil
}

// confidenceValue maps the SQLite text confidence value back to the domain enum.
func confidenceValue(s string) domain.Confidence {
	switch s {
	case "definite":
		return domain.Definite
	case "strong":
		return domain.Strong
	case "probable":
		return domain.Probable
	default:
		return domain.Unresolved
	}
}
