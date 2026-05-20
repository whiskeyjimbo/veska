package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// GraphRepo is the SQLite-backed adapter for ports.GraphStorage. It reads and
// writes the `nodes` and `edges` tables created by migration 0001 (plus the
// signature columns from 0005).
//
// Writes take the write-capable handle (the single-writer hot pool in the
// daemon); reads take the read pool so graph traversals do not contend with
// promotion. Its upsert SQL mirrors the column set and ON CONFLICT semantics
// of the Promoter so a GraphRepo write and a promotion write are compatible.
type GraphRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// Compile-time assertion that GraphRepo satisfies the port.
var _ ports.GraphStorage = (*GraphRepo)(nil)

// NewGraphRepo constructs a GraphRepo. readDB serves LoadGraph/FindNodes/
// GetNode; writeDB serves SaveNode/SaveEdge/DeleteFile.
func NewGraphRepo(readDB, writeDB *sql.DB) *GraphRepo {
	return &GraphRepo{readDB: readDB, writeDB: writeDB}
}

// nodeColumns is the SELECT column list shared by GetNode and FindNodes. It
// matches the subset of `nodes` columns needed to rehydrate a domain.Node.
const nodeColumns = `node_id, symbol_path, file_path, kind, language,
	line_start, line_end, content_hash, signature`

// scanNode rehydrates a domain.Node from a row selected with nodeColumns.
// Nullable columns (line_start/line_end, language, signature) are read into
// sql.Null* and only fed to functional options when valid.
func scanNode(s interface {
	Scan(dest ...any) error
}) (*domain.Node, error) {
	var (
		id, symbolPath, filePath, kind string
		language                       sql.NullString
		lineStart, lineEnd             sql.NullInt64
		contentHash                    sql.NullString
		signature                      sql.NullString
	)
	if err := s.Scan(&id, &symbolPath, &filePath, &kind, &language,
		&lineStart, &lineEnd, &contentHash, &signature); err != nil {
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

	n, err := domain.NewNode(id, filePath, symbolPath, domain.NodeKind(kind), opts...)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: rehydrate node %q: %w", id, err)
	}
	return n, nil
}

// maxSnippetBytes bounds the node body persisted into nodes.snippet. The body
// feeds embed-text projection, so it is capped to keep embed cost and snippet
// storage bounded and uniform — matching the recallprojection harness cap.
const maxSnippetBytes = 2000

// capSnippet trims s to at most maxSnippetBytes on a UTF-8 rune boundary so the
// stored snippet never contains a broken rune.
func capSnippet(s string) string {
	if len(s) <= maxSnippetBytes {
		return s
	}
	cut := maxSnippetBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// SaveNode inserts or replaces a node row keyed on (node_id, branch). The
// column set and ON CONFLICT clause mirror the Promoter so a GraphRepo write
// is interchangeable with a promotion write.
func (r *GraphRepo) SaveNode(ctx context.Context, repoID, branch string, n *domain.Node) error {
	if n == nil {
		return nil
	}
	const stmt = `
INSERT INTO nodes
	(node_id, branch, repo_id, language, kind, symbol_path, file_path,
	 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
	 signature, snippet, prev_signature)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON CONFLICT(node_id, branch) DO UPDATE SET
	repo_id          = excluded.repo_id,
	language         = excluded.language,
	kind             = excluded.kind,
	symbol_path      = excluded.symbol_path,
	file_path        = excluded.file_path,
	line_start       = excluded.line_start,
	line_end         = excluded.line_end,
	content_hash     = excluded.content_hash,
	last_promoted_at = excluded.last_promoted_at,
	actor_id         = excluded.actor_id,
	actor_kind       = excluded.actor_kind,
	signature        = excluded.signature,
	snippet          = excluded.snippet`

	var lineStart, lineEnd any
	if n.Lines != nil {
		lineStart, lineEnd = n.Lines.Start, n.Lines.End
	}
	// language is NOT NULL in the schema; mirror the Promoter and write the
	// empty string when the parser did not populate it.
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
	// RawContent is captured by the parser; a node without it stores NULL.
	var snippet any
	if n.RawContent != nil {
		snippet = capSnippet(*n.RawContent)
	}

	now := time.Now().UnixMilli()
	if _, err := r.writeDB.ExecContext(ctx, stmt,
		string(n.ID), branch, repoID, language, string(n.Kind),
		n.Name, n.Path, lineStart, lineEnd, contentHash, now,
		string(domain.ActorKindSystem), string(domain.ActorKindSystem),
		signature, snippet,
	); err != nil {
		return fmt.Errorf("graph_repo: save node %q: %w", n.ID, err)
	}
	return nil
}

// SaveEdge inserts or replaces an edge row keyed on (edge_id, branch). The
// edge_id is the deterministic hash of (Src, Kind, Tgt), so the upsert key is
// effectively (From, To, Kind) per the port contract.
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

// DeleteFile removes every node and edge whose source file is filePath for
// the given (repoID, branch). Edges are deleted explicitly first: the FK
// ON DELETE CASCADE only fires when SQLite foreign-key enforcement is on, so
// removing edges by their endpoints' file makes the behaviour deterministic.
func (r *GraphRepo) DeleteFile(ctx context.Context, repoID, branch, filePath string) error {
	tx, err := r.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("graph_repo: delete file begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Delete edges incident to any node in the file (as src or dst).
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

// LoadGraph builds the full in-memory Graph for (repoID, branch). It always
// returns a non-nil Graph — an empty one when no nodes are stored. Edges whose
// endpoints are both present are added; dangling edges are skipped.
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
		`SELECT src_node_id, dst_node_id, kind, confidence
		 FROM edges WHERE repo_id = ? AND branch = ?`,
		repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("graph_repo: load edges: %w", err)
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var src, dst, kind, conf string
		if err := edgeRows.Scan(&src, &dst, &kind, &conf); err != nil {
			return nil, fmt.Errorf("graph_repo: scan edge: %w", err)
		}
		e, err := domain.NewEdge(domain.NodeID(src), domain.NodeID(dst),
			domain.EdgeKind(kind), domain.WithConfidence(confidenceValue(conf)))
		if err != nil {
			return nil, fmt.Errorf("graph_repo: rehydrate edge: %w", err)
		}
		// Skip edges whose endpoints are absent — AddEdge requires both.
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

// FindNodes returns every node whose symbol name (symbol_path column) exactly
// equals symbolName for (repoID, branch).
func (r *GraphRepo) FindNodes(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE repo_id = ? AND branch = ? AND symbol_path = ?`,
		repoID, branch, symbolName)
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
