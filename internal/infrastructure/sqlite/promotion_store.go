// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// edgeSrcLine returns the 1-indexed SourceLine value when set, or NULL otherwise.
// Persisting NULL keeps the database backward compatible for legacy edges.
func edgeSrcLine(e *domain.Edge) any {
	if e == nil || e.SourceLine == nil {
		return nil
	}
	return *e.SourceLine
}

var _ application.PromotionStore = (*PromotionStore)(nil)

// PromotionStore is the SQLite adapter for the application.PromotionStore port.
// Sinks are registered at construction time to keep the core Promote
// transaction logic extensible without modification.
type PromotionStore struct {
	writeDB        *sql.DB
	sinks          []PromotionSink
	reviewEnabled  bool
	summaryEnabled bool
	workKinds      []string
}

// PromotionStoreOption configures a PromotionStore at construction time.
type PromotionStoreOption func(*PromotionStore)

// WithReviewEnabled configures whether the optional WorkKindReview lane is enqueued.
func WithReviewEnabled(enabled bool) PromotionStoreOption {
	return func(s *PromotionStore) { s.reviewEnabled = enabled }
}

// WithSummaryEnabled configures whether the optional WorkKindSummary lane is enqueued.
func WithSummaryEnabled(enabled bool) PromotionStoreOption {
	return func(s *PromotionStore) { s.summaryEnabled = enabled }
}

// NewPromotionStore constructs a PromotionStore with the given database handle
// and sinks.
func NewPromotionStore(writeDB *sql.DB, sinks []PromotionSink, opts ...PromotionStoreOption) *PromotionStore {
	s := &PromotionStore{
		writeDB: writeDB,
		sinks:   sinks,
	}
	for _, o := range opts {
		o(s)
	}
	s.workKinds = application.PromotionWorkKinds(s.reviewEnabled, s.summaryEnabled)
	return s
}

// Promote executes all promotion phases within a single serializable transaction.
func (s *PromotionStore) Promote(ctx context.Context, batch application.PromotionBatch) error {
	rootPath, modulePath, err := s.lookupRepo(ctx, batch.RepoID)
	if err != nil {
		return err
	}

	// Short-circuit early if there are no files to promote.
	if len(batch.Files) == 0 {
		return nil
	}

	tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("promoter: begin tx: %w", err)
	}

	p, err := s.newPromotion(ctx, tx, batch, rootPath, modulePath)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer p.closeStmts()

	// Sinks and node updates are executed first so that nodes exist before
	// edge resolution binds caller-callee links.
	for _, phase := range []func(context.Context) error{
		p.promoteFiles,
		p.enqueueWiki,
		p.insertParserEdges,
		p.resolveIntraPackageCalls,
		p.resolveCrossPackageCalls,
		p.advanceRepoSHA,
	} {
		if err := phase(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("promoter: commit: %w", err)
	}
	return nil
}

// lookupRepo retrieves registration details for a repository, returning an error
// if it is unregistered.
func (s *PromotionStore) lookupRepo(ctx context.Context, repoID string) (root, module string, err error) {
	var rootPath, modulePath sql.NullString
	qerr := s.writeDB.QueryRowContext(ctx,
		`SELECT root_path, module_path FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&rootPath, &modulePath)
	if qerr == sql.ErrNoRows {
		return "", "", application.ErrUnregisteredRepo{RepoID: repoID}
	}
	if qerr != nil {
		return "", "", fmt.Errorf("promoter: check repo registration: %w", qerr)
	}
	return rootPath.String, modulePath.String, nil
}

// promotion coordinates state and shared prepared statements for a single promotion.
type promotion struct {
	s          *PromotionStore
	tx         *sql.Tx
	batch      application.PromotionBatch
	repoID     string
	branch     string
	now        int64
	rootPath   string
	modulePath string

	del        *sql.Stmt
	ins        *sql.Stmt
	prevSigSel *sql.Stmt
	queue      *sql.Stmt
	delImports *sql.Stmt
	insImports *sql.Stmt
	edge       *sql.Stmt
}

func (s *PromotionStore) newPromotion(ctx context.Context, tx *sql.Tx, batch application.PromotionBatch, root, module string) (*promotion, error) {
	p := &promotion{
		s:          s,
		tx:         tx,
		batch:      batch,
		repoID:     batch.RepoID,
		branch:     batch.Branch,
		now:        batch.PromotedAt,
		rootPath:   root,
		modulePath: module,
	}
	if err := p.prepareStmts(ctx); err != nil {
		return nil, err
	}
	for _, sink := range s.sinks {
		if err := sink.Prepare(ctx, tx); err != nil {
			return nil, fmt.Errorf("promoter: prepare sink: %w", err)
		}
	}
	return p, nil
}

// prepare compiles a prepared statement within the transaction context.
func prepare(ctx context.Context, tx *sql.Tx, label, query string) (*sql.Stmt, error) {
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("promoter: prepare %s: %w", label, err)
	}
	return stmt, nil
}

// prepareStmts compiles statements reused during the promotion.
func (p *promotion) prepareStmts(ctx context.Context) error {
	var err error
	if p.del, err = prepare(ctx, p.tx, "delete",
		`DELETE FROM nodes WHERE file_path = ? AND branch = ? AND repo_id = ?`); err != nil {
		return err
	}
	if p.ins, err = prepare(ctx, p.tx, "insert", `
		INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, structural_hash, last_promoted_at, actor_id, actor_kind,
			 signature, snippet, prev_signature, exported)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`); err != nil {
		return err
	}
	if p.prevSigSel, err = prepare(ctx, p.tx, "prev-sig select", `
		SELECT node_id, signature FROM nodes
		WHERE file_path = ? AND branch = ? AND repo_id = ?`); err != nil {
		return err
	}
	if p.queue, err = prepare(ctx, p.tx, "queue insert", `
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`); err != nil {
		return err
	}
	if p.delImports, err = prepare(ctx, p.tx, "file_imports delete",
		`DELETE FROM file_imports WHERE repo_id = ? AND branch = ? AND file_path = ?`); err != nil {
		return err
	}
	if p.insImports, err = prepare(ctx, p.tx, "file_imports insert", `
		INSERT INTO file_imports
			(repo_id, branch, file_path, import_path, alias, language, last_promoted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, branch, file_path, import_path) DO NOTHING`); err != nil {
		return err
	}
	// Edges use INSERT OR IGNORE to ensure promotion is idempotent.
	if p.edge, err = prepare(ctx, p.tx, "edge insert", `
		INSERT INTO edges
			(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at, src_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(edge_id, branch) DO NOTHING`); err != nil {
		return err
	}
	return nil
}

func (p *promotion) closeStmts() {
	for _, st := range []*sql.Stmt{p.del, p.ins, p.prevSigSel, p.queue, p.delImports, p.insImports, p.edge} {
		if st != nil {
			_ = st.Close()
		}
	}
}

func (p *promotion) insertEdge(ctx context.Context, e *domain.Edge) error {
	_, err := p.edge.ExecContext(ctx,
		e.ID, p.branch, p.repoID,
		string(e.Src), string(e.Tgt),
		string(e.Kind), confidenceText(e.Confidence), p.now,
		edgeSrcLine(e),
	)
	return err
}

func (p *promotion) promoteFiles(ctx context.Context) error {
	for _, file := range p.batch.Files {
		if err := p.promoteFile(ctx, file); err != nil {
			return err
		}
	}
	return nil
}

func (p *promotion) promoteFile(ctx context.Context, file application.PromotionFile) error {
	prevSig, err := p.capturePrevSignatures(ctx, file.Path)
	if err != nil {
		return err
	}
	// Pre-delete hooks are run before database deletions so sinks can query
	// active node rows.
	for _, sink := range p.s.sinks {
		if err := sink.BeforeNodeDelete(ctx, p.tx, p.branch, p.repoID, file.Path); err != nil {
			return fmt.Errorf("promoter: sink before-delete for %q: %w", file.Path, err)
		}
	}
	if _, err := p.del.ExecContext(ctx, file.Path, p.branch, p.repoID); err != nil {
		return fmt.Errorf("promoter: delete nodes for %q: %w", file.Path, err)
	}
	if err := p.syncFileImports(ctx, file); err != nil {
		return err
	}
	if err := p.insertFileNodes(ctx, file, prevSig); err != nil {
		return err
	}
	return p.enqueueFileWork(ctx, file.Path)
}

// capturePrevSignatures snapshots existing signatures before deletion to carry
// them forward, distinguishing unknown signatures (NULL) from empty ones.
func (p *promotion) capturePrevSignatures(ctx context.Context, filePath string) (map[string]*string, error) {
	prevSig := make(map[string]*string)
	rows, err := p.prevSigSel.QueryContext(ctx, filePath, p.branch, p.repoID)
	if err != nil {
		return nil, fmt.Errorf("promoter: select prev signatures for %q: %w", filePath, err)
	}
	for rows.Next() {
		var nodeID string
		var sig sql.NullString
		if err := rows.Scan(&nodeID, &sig); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("promoter: scan prev signature for %q: %w", filePath, err)
		}
		if sig.Valid {
			v := sig.String
			prevSig[nodeID] = &v
		} else {
			prevSig[nodeID] = nil
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("promoter: iterate prev signatures for %q: %w", filePath, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("promoter: close prev signatures for %q: %w", filePath, err)
	}
	return prevSig, nil
}

// syncFileImports synchronizes external import definitions for the promoted file.
func (p *promotion) syncFileImports(ctx context.Context, file application.PromotionFile) error {
	if _, err := p.delImports.ExecContext(ctx, p.repoID, p.branch, file.Path); err != nil {
		return fmt.Errorf("promoter: delete file_imports for %q: %w", file.Path, err)
	}
	for alias, importPath := range file.Imports {
		_, ownModule := modulePackageDir(p.modulePath, importPath)
		if importPath == "" || !isExternalModulePath(importPath) || ownModule {
			continue
		}
		if _, err := p.insImports.ExecContext(ctx,
			p.repoID, p.branch, file.Path, importPath, alias, "go", p.now,
		); err != nil {
			return fmt.Errorf("promoter: insert file_imports for %q (%s): %w", file.Path, importPath, err)
		}
	}
	return nil
}

func (p *promotion) insertFileNodes(ctx context.Context, file application.PromotionFile, prevSig map[string]*string) error {
	for _, n := range file.Nodes {
		if err := p.insertNode(ctx, n, prevSig); err != nil {
			return err
		}
	}
	return nil
}

// insertNode upserts a node and triggers downstream sink writes.
func (p *promotion) insertNode(ctx context.Context, n *domain.Node, prevSig map[string]*string) error {
	var prev any
	if ps, ok := prevSig[string(n.ID)]; ok && ps != nil {
		prev = *ps
	}
	lineStart, lineEnd := nodeLines(n)
	if _, err := p.ins.ExecContext(ctx,
		string(n.ID),
		p.branch,
		p.repoID,
		nodeLanguage(n),
		string(n.Kind),
		n.Name,
		n.Path,
		lineStart,
		lineEnd,
		nodeContentHash(n),
		nodeStructuralHash(n),
		p.now,
		p.batch.Actor.ID,
		string(p.batch.Actor.Kind),
		nodeSignature(n),
		nodeSnippet(n),
		prev,
		nodeExported(n),
	); err != nil {
		// Detailed metadata is included in unique constraint violations to help
		// diagnose parser duplicate node definitions.
		return fmt.Errorf("promoter: insert node %q (kind=%s name=%q path=%q lines=%v): %w",
			n.ID, n.Kind, n.Name, n.Path, n.Lines, err)
	}
	nw := nodeWrite{
		NodeID: string(n.ID),
		Branch: p.branch,
		RepoID: p.repoID,
		Kind:   string(n.Kind),
		Symbol: n.Name,
	}
	for _, sink := range p.s.sinks {
		if err := sink.AfterNodeInsert(ctx, p.tx, nw, p.now); err != nil {
			return fmt.Errorf("promoter: sink after-insert for %q: %w", n.ID, err)
		}
	}
	return nil
}

func (p *promotion) enqueueFileWork(ctx context.Context, filePath string) error {
	for _, wk := range p.s.workKinds {
		if _, err := p.queue.ExecContext(ctx,
			p.batch.GitSHA, p.repoID, p.branch, p.batch.GitSHA, wk, filePath, p.now,
		); err != nil {
			return fmt.Errorf("promoter: enqueue %q for %q: %w", wk, filePath, err)
		}
	}
	return nil
}

// enqueueWiki registers a repository-scoped wiki task. A single row suffices
// because the wiki lane processes the entire repository surface at once.
func (p *promotion) enqueueWiki(ctx context.Context) error {
	if _, err := p.queue.ExecContext(ctx,
		p.batch.GitSHA, p.repoID, p.branch, p.batch.GitSHA, string(ports.WorkKindWiki), "", p.now,
	); err != nil {
		return fmt.Errorf("promoter: enqueue wiki: %w", err)
	}
	return nil
}

// insertParserEdges persists edges produced during the initial parsing phase.
func (p *promotion) insertParserEdges(ctx context.Context) error {
	for _, file := range p.batch.Files {
		for _, e := range file.Edges {
			if e == nil {
				continue
			}
			if err := p.insertEdge(ctx, e); err != nil {
				return fmt.Errorf("promoter: insert edge %q: %w", e.ID, err)
			}
		}
	}
	return nil
}

// advanceRepoSHA updates the latest promoted commit hash and active branch.
func (p *promotion) advanceRepoSHA(ctx context.Context) error {
	if p.batch.GitSHA == "" {
		return nil
	}
	var err error
	if p.branch != "" {
		_, err = p.tx.ExecContext(ctx,
			`UPDATE repos SET last_promoted_sha = ?, active_branch = ? WHERE repo_id = ?`,
			p.batch.GitSHA, p.branch, p.repoID,
		)
	} else {
		_, err = p.tx.ExecContext(ctx,
			`UPDATE repos SET last_promoted_sha = ? WHERE repo_id = ?`,
			p.batch.GitSHA, p.repoID,
		)
	}
	if err != nil {
		return fmt.Errorf("promoter: advance last_promoted_sha: %w", err)
	}
	return nil
}

func nodeLanguage(n *domain.Node) string {
	if n.Language == nil {
		return ""
	}
	return *n.Language
}

func nodeLines(n *domain.Node) (lineStart, lineEnd any) {
	if n.Lines == nil {
		return nil, nil
	}
	return n.Lines.Start, n.Lines.End
}

func nodeStructuralHash(n *domain.Node) any {
	if n.StructuralHash == nil {
		return nil
	}
	return string(*n.StructuralHash)
}

func nodeContentHash(n *domain.Node) string {
	if n.ContentHash == nil {
		return ""
	}
	return string(*n.ContentHash)
}

func nodeSignature(n *domain.Node) any {
	if n.Signature == nil {
		return nil
	}
	return *n.Signature
}

func nodeExported(n *domain.Node) any {
	if n.Exported == nil {
		return nil
	}
	if *n.Exported {
		return 1
	}
	return 0
}
