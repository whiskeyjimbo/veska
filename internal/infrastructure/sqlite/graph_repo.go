// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// GraphRepo implements ports.GraphStorage and ports.GraphReader. It separates
// read and write operations across separate database handles to prevent graph
// traversals from contending with the single-writer promotion connection. The
// upsert SQL logic is designed to match the Promoter's schema requirements.
type GraphRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// Compile-time assertion that GraphRepo satisfies both the write and read ports.
var (
	_ ports.GraphStorage = (*GraphRepo)(nil)
	_ ports.GraphReader  = (*GraphRepo)(nil)
)

// NewGraphRepo constructs a GraphRepo.
func NewGraphRepo(readDB, writeDB *sql.DB) *GraphRepo {
	return &GraphRepo{readDB: readDB, writeDB: writeDB}
}

// nodeColumns lists the columns needed to rehydrate a domain.Node.
const nodeColumns = `node_id, symbol_path, file_path, kind, language,
	line_start, line_end, content_hash, signature, exported, external, short_summary`

// scanNode rehydrates a domain.Node from a query row.
func scanNode(s interface {
	Scan(dest ...any) error
},
) (*domain.Node, error) {
	var (
		id, symbolPath, filePath, kind string
		language                       sql.NullString
		lineStart, lineEnd             sql.NullInt64
		contentHash                    sql.NullString
		signature                      sql.NullString
		exported                       sql.NullBool
		external                       sql.NullBool
		shortSummary                   sql.NullString
	)
	if err := s.Scan(&id, &symbolPath, &filePath, &kind, &language,
		&lineStart, &lineEnd, &contentHash, &signature, &exported, &external, &shortSummary); err != nil {
		return nil, err
	}

	opts := make([]domain.NodeOption, 0, 4)
	if language.Valid && language.String != "" {
		opts = append(opts, domain.WithLanguage(language.String))
	}
	if lineStart.Valid && lineEnd.Valid && lineStart.Int64 >= 1 && lineEnd.Int64 >= 1 {
		opts = append(opts, domain.WithLines(domain.LineRange{
			Start: int(lineStart.Int64),
			End:   int(lineEnd.Int64),
		}))
	}
	if contentHash.Valid && contentHash.String != "" {
		opts = append(opts, domain.WithContentHash(domain.ContentHash(contentHash.String)))
	}
	if signature.Valid {
		opts = append(opts, domain.WithSignature(signature.String))
	}
	if exported.Valid {
		opts = append(opts, domain.WithExported(exported.Bool))
	}
	if external.Valid && external.Bool {
		opts = append(opts, domain.WithExternal(true))
	}
	if shortSummary.Valid && shortSummary.String != "" {
		// Stored summaries are written ≤ MaxShortSummaryRunes by the lane; clamp
		// defensively so a longer legacy value cannot fail rehydration.
		opts = append(opts, domain.WithShortSummary(domain.TruncateRunes(shortSummary.String, domain.MaxShortSummaryRunes)))
	}

	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: filePath, Name: symbolPath, Kind: domain.NodeKind(kind)}, opts...)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: rehydrate node %q: %w", id, err)
	}
	return n, nil
}

// SaveNode inserts or replaces a node row. Its schema fields and conflict
// resolution match the Promoter to ensure compatibility between independent
// writes and promotion events.
func (r *GraphRepo) SaveNode(ctx context.Context, repoID, branch string, n *domain.Node) error {
	if n == nil {
		return nil
	}
	const stmt = `
INSERT INTO nodes
	(node_id, branch, repo_id, language, kind, symbol_path, file_path,
	 line_start, line_end, content_hash, structural_hash, last_promoted_at, actor_id, actor_kind,
	 signature, snippet, prev_signature, exported)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(node_id, branch) DO UPDATE SET
	repo_id          = excluded.repo_id,
	language         = excluded.language,
	kind             = excluded.kind,
	symbol_path      = excluded.symbol_path,
	file_path        = excluded.file_path,
	line_start       = excluded.line_start,
	line_end         = excluded.line_end,
	content_hash     = excluded.content_hash,
	structural_hash  = excluded.structural_hash,
	last_promoted_at = excluded.last_promoted_at,
	actor_id         = excluded.actor_id,
	actor_kind       = excluded.actor_kind,
	signature        = excluded.signature,
	snippet          = excluded.snippet,
	exported         = excluded.exported`

	var lineStart, lineEnd any
	if n.Lines != nil {
		lineStart, lineEnd = n.Lines.Start, n.Lines.End
	}
	// Language is NOT NULL in the schema, so an empty string is written if it
	// was not populated by the parser.
	language := ""
	if n.Language != nil {
		language = *n.Language
	}
	contentHash := ""
	if n.ContentHash != nil {
		contentHash = string(*n.ContentHash)
	}
	var signature any
	if n.Signature != nil {
		signature = *n.Signature
	}
	snippet := nodeSnippet(n)

	now := time.Now().UnixMilli()
	if _, err := r.writeDB.ExecContext(ctx, stmt,
		string(n.ID), branch, repoID, language, string(n.Kind),
		n.Name, n.Path, lineStart, lineEnd, contentHash, nodeStructuralHash(n), now,
		string(domain.ActorKindSystem), string(domain.ActorKindSystem),
		signature, snippet, nodeExported(n),
	); err != nil {
		return fmt.Errorf("graph_repo: save node %q: %w", n.ID, err)
	}
	return nil
}

// UpsertExternalRepo writes a synthetic repository row for a vendor-indexed
// module so the cross-repository edge resolver can match it. The root path
// must point to the root of the vendored module to ensure subpackage relative
// paths are computed correctly. Synthetic rows bypass the identity resolution
// chain.
func (r *GraphRepo) UpsertExternalRepo(ctx context.Context, repoID, rootPath, modulePath, branch string) error {
	_, err := r.writeDB.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO UPDATE SET
			root_path     = excluded.root_path,
			active_branch = excluded.active_branch,
			module_path   = excluded.module_path`,
		repoID, rootPath, time.Now().Unix(), branch, modulePath,
	)
	if err != nil {
		return fmt.Errorf("graph_repo: upsert external repo %q: %w", repoID, err)
	}
	return nil
}

// SaveExternalNode inserts or replaces a dependency node, marking it as external
// so first-party views can filter it out during queries.
func (r *GraphRepo) SaveExternalNode(ctx context.Context, repoID, branch string, n *domain.Node) error {
	if n == nil {
		return nil
	}
	const stmt = `
INSERT INTO nodes
	(node_id, branch, repo_id, language, kind, symbol_path, file_path,
	 line_start, line_end, content_hash, structural_hash, last_promoted_at, actor_id, actor_kind,
	 signature, snippet, prev_signature, exported, external)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, 1)
ON CONFLICT(node_id, branch) DO UPDATE SET
	repo_id          = excluded.repo_id,
	language         = excluded.language,
	kind             = excluded.kind,
	symbol_path      = excluded.symbol_path,
	file_path        = excluded.file_path,
	line_start       = excluded.line_start,
	line_end         = excluded.line_end,
	content_hash     = excluded.content_hash,
	structural_hash  = excluded.structural_hash,
	last_promoted_at = excluded.last_promoted_at,
	actor_id         = excluded.actor_id,
	actor_kind       = excluded.actor_kind,
	signature        = excluded.signature,
	snippet          = excluded.snippet,
	exported         = excluded.exported,
	external         = 1`

	var lineStart, lineEnd any
	if n.Lines != nil {
		lineStart, lineEnd = n.Lines.Start, n.Lines.End
	}
	language := ""
	if n.Language != nil {
		language = *n.Language
	}
	contentHash := ""
	if n.ContentHash != nil {
		contentHash = string(*n.ContentHash)
	}
	var signature any
	if n.Signature != nil {
		signature = *n.Signature
	}
	snippet := nodeSnippet(n)

	now := time.Now().UnixMilli()
	if _, err := r.writeDB.ExecContext(ctx, stmt,
		string(n.ID), branch, repoID, language, string(n.Kind),
		n.Name, n.Path, lineStart, lineEnd, contentHash, nodeStructuralHash(n), now,
		string(domain.ActorKindSystem), string(domain.ActorKindSystem),
		signature, snippet, nodeExported(n),
	); err != nil {
		return fmt.Errorf("graph_repo: save external node %q: %w", n.ID, err)
	}
	return nil
}

// SaveEdge inserts or replaces an edge row. The edge ID is computed
// deterministically from the source, destination, and kind.
func (r *GraphRepo) SaveEdge(ctx context.Context, repoID, branch string, e *domain.Edge) error {
	if e == nil {
		return nil
	}
	const stmt = `
INSERT INTO edges
	(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(edge_id, branch) DO UPDATE SET
	repo_id          = excluded.repo_id,
	src_node_id      = excluded.src_node_id,
	dst_node_id      = excluded.dst_node_id,
	kind             = excluded.kind,
	confidence       = excluded.confidence,
	last_promoted_at = excluded.last_promoted_at`

	now := time.Now().UnixMilli()
	if _, err := r.writeDB.ExecContext(ctx, stmt,
		e.ID, branch, repoID, string(e.Src), string(e.Tgt),
		string(e.Kind), confidenceText(e.Confidence), now,
	); err != nil {
		return fmt.Errorf("graph_repo: save edge %q: %w", e.ID, err)
	}
	return nil
}

// DeleteFile deletes all nodes and edges associated with the specified file.
// Edges are removed explicitly rather than relying on CASCADE DELETE because
// foreign key enforcement is not guaranteed to be active.
func (r *GraphRepo) DeleteFile(ctx context.Context, repoID, branch, filePath string) error {
	tx, err := r.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("graph_repo: delete file begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges
		 WHERE repo_id = ? AND branch = ?
		   AND (src_node_id IN (SELECT node_id FROM nodes
		                        WHERE repo_id = ? AND branch = ? AND file_path = ?)
		     OR dst_node_id IN (SELECT node_id FROM nodes
		                        WHERE repo_id = ? AND branch = ? AND file_path = ?))`,
		repoID, branch, repoID, branch, filePath, repoID, branch, filePath,
	); err != nil {
		return fmt.Errorf("graph_repo: delete edges for %q: %w", filePath, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM nodes WHERE repo_id = ? AND branch = ? AND file_path = ?`,
		repoID, branch, filePath,
	); err != nil {
		return fmt.Errorf("graph_repo: delete nodes for %q: %w", filePath, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("graph_repo: delete file commit: %w", err)
	}
	return nil
}

// LoadGraph reconstructs the complete in-memory graph. Edges with missing
// endpoints are skipped since endpoints must exist prior to edge insertion.
func (r *GraphRepo) LoadGraph(ctx context.Context, repoID, branch string) (*domain.Graph, error) {
	g, err := domain.NewGraph(repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: new graph: %w", err)
	}

	nodeRows, err := r.readDB.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE repo_id = ? AND branch = ?`,
		repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: load nodes: %w", err)
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		n, err := scanNode(nodeRows)
		if err != nil {
			return nil, fmt.Errorf("graph_repo: scan node: %w", err)
		}
		if err := g.AddNode(n); err != nil {
			return nil, fmt.Errorf("graph_repo: add node: %w", err)
		}
	}
	if err := nodeRows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate nodes: %w", err)
	}

	edgeRows, err := r.readDB.QueryContext(ctx,
		`SELECT src_node_id, dst_node_id, kind, confidence, src_line
		 FROM edges WHERE repo_id = ? AND branch = ?`,
		repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: load edges: %w", err)
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var src, dst, kind, conf string
		var srcLine sql.NullInt64
		if err := edgeRows.Scan(&src, &dst, &kind, &conf, &srcLine); err != nil {
			return nil, fmt.Errorf("graph_repo: scan edge: %w", err)
		}
		opts := []domain.EdgeOption{domain.WithConfidence(confidenceValue(conf))}
		if srcLine.Valid && srcLine.Int64 > 0 {
			opts = append(opts, domain.WithSourceLine(int(srcLine.Int64)))
		}
		e, err := domain.NewEdge(domain.EdgeSpec{
			Src:  domain.NodeID(src),
			Tgt:  domain.NodeID(dst),
			Kind: domain.EdgeKind(kind),
		}, opts...)
		if err != nil {
			return nil, fmt.Errorf("graph_repo: rehydrate edge: %w", err)
		}
		if _, ok := g.Node(e.Src); !ok {
			continue
		}
		if _, ok := g.Node(e.Tgt); !ok {
			continue
		}
		if err := g.AddEdge(e); err != nil {
			return nil, fmt.Errorf("graph_repo: add edge: %w", err)
		}
	}
	if err := edgeRows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate edges: %w", err)
	}

	return g, nil
}

// NodeSnippets returns the stored source body for every node in repoID/branch,
// keyed by node_id. LoadGraph deliberately omits the snippet column (the hot
// traversal path only needs structure), so the graph-export snapshot fetches
// the bodies separately to carry node source for the viewer. Nodes with a NULL
// or empty snippet are omitted from the map.
func (r *GraphRepo) NodeSnippets(ctx context.Context, repoID, branch string) (map[string]string, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT node_id, snippet FROM nodes
		 WHERE repo_id = ? AND branch = ? AND snippet IS NOT NULL AND snippet <> ''`,
		repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: load snippets: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, snippet string
		if err := rows.Scan(&id, &snippet); err != nil {
			return nil, fmt.Errorf("graph_repo: scan snippet: %w", err)
		}
		out[id] = snippet
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph_repo: iterate snippets: %w", err)
	}
	return out, nil
}
