package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// escapeGlob wraps GLOB metacharacters (*, ?, [) in a literal-character
// class so an identifier embedded in a GLOB pattern matches itself. GLOB
// has no ESCAPE clause, so the only safe way is the [X] form.
//
// Identifiers in supported languages are [A-Za-z0-9_.] (plus a leading $ in
// some), so the *, ?, and [ characters do not appear inside an identifier in
// practice; this is defence-in-depth for fuzz inputs / odd languages.
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

func (r *GraphRepo) FindNodes(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error) {
	// Match the fully-qualified symbol_path exactly, OR an unqualified name
	// against the trailing segment so "Start" finds "Server.Start" instead
	// of silently returning nothing . Exact matches sort first.
	//
	// solov2-xcb1: SQLite LIKE is case-INsensitive for ASCII regardless of
	// COLLATE, so a search for "Run" used to also match
	// "FSNotifyWatcher.run" — a distinct symbol. Identifiers are
	// case-significant in every supported language, so we use GLOB (which
	// is byte-exact) for the suffix match. GLOB uses *,? wildcards (vs
	// LIKE's %,_), and treats [ ] as character classes — escape those in
	// the user-supplied identifier so a literal "[" isn't treated as a
	// class opener.
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

// NodesForFile returns every node whose file_path equals filePath for
// (repoID, branch). Backs eng_get_file_nodes. Returns an empty slice (not an
// error) when the file has no promoted nodes, matching the port contract.
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

// GetNode retrieves a single node by ID for (repoID, branch). A missing node
// returns (nil, nil) — the caller treats absence as a normal outcome.
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

// GetNodeSnippet returns the persisted (capped) body text for a single
// node. Returns ("", nil) when the row is missing or the snippet column
// stored NULL — callers treat the empty result as "no snippet available"
// and fall back to conservative defaults. The returned text is capped to
// maxSnippetBytes by the write path, so callers must not assume it equals
// the full source. Backs the eng_get_call_chain seed-body discriminator
// (solov2-izh6.22).
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

// FindNodeByID retrieves the first node matching the content-hashed id across
// every (repo_id, branch). node_id is a sha256 content hash so collisions
// across repos/branches are vanishingly rare; LIMIT 1 returns one
// deterministic row. Returns (nil, nil) when no node matches .
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

// confidenceValue is the inverse of confidenceText: it maps the TEXT column
// value back onto the domain Confidence enum. An unknown string maps to
// Unresolved.
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
