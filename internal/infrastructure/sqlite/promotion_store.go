package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// edgeSrcLine returns the SQL bind value for the edges.src_line column —
// the edge's 1-indexed SourceLine when set, NULL otherwise (solov2-izh6.31).
// Persisting NULL for unknown lines keeps the migration backward-
// compatible: legacy rows read as NULL and renderers fall back to
// today's caller-node-line behaviour for both.
func edgeSrcLine(e *domain.Edge) any {
	if e == nil || e.SourceLine == nil {
		return nil
	}
	return *e.SourceLine
}

// ucSrcLine returns the SQL bind value for cross_repo_edge_stubs.src_line
// — the UnresolvedCall's SrcLine when non-zero, NULL otherwise. The
// stub-resolution step in graph_repo (cross-repo stub → resolved Edge)
// copies this value into the resulting Edge.SourceLine (solov2-izh6.31).
func ucSrcLine(uc domain.UnresolvedCall) any {
	if uc.SrcLine <= 0 {
		return nil
	}
	return uc.SrcLine
}

// buildPackageSymbolMap groups symbol-name → node_id by file directory.
// Go's "one package per directory" convention means a single map per
// dir is sufficient for resolving same-package, cross-file calls
// (solov2-2at). The values shadow on conflict (last file wins) — only
// matters when two files in the same dir export the same symbol name,
// which is illegal Go anyway.
func buildPackageSymbolMap(batch application.PromotionBatch) map[string]map[string]domain.NodeID {
	out := make(map[string]map[string]domain.NodeID)
	for _, file := range batch.Files {
		dir := filepath.Dir(file.Path)
		bucket, ok := out[dir]
		if !ok {
			bucket = make(map[string]domain.NodeID)
			out[dir] = bucket
		}
		for _, n := range file.Nodes {
			if n == nil {
				continue
			}
			bucket[n.Name] = n.ID
		}
	}
	return out
}

// moduleRelDir returns path's directory relative to the repo's working-tree
// root, in slash form. Node/file paths reach promotion in a mix of absolute
// (cold scan) and repo-relative (incremental commit) forms; normalising both
// against root gives a single package-key space for cross-package resolution
// (solov2-xc51). The module-root package maps to "".
func moduleRelDir(path, root string) string {
	p := filepath.ToSlash(path)
	if root != "" {
		if rest, ok := strings.CutPrefix(p, filepath.ToSlash(root)+"/"); ok {
			p = rest
		}
	}
	dir := filepath.ToSlash(filepath.Dir(p))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// modulePackageDir maps a Go import path to its package directory relative to
// the module root. inModule is false when importPath is not under modulePath
// (stdlib or another module — handled as a cross-repo stub instead).
func modulePackageDir(modulePath, importPath string) (relDir string, inModule bool) {
	if modulePath == "" {
		return "", false
	}
	if importPath == modulePath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(importPath, modulePath+"/"); ok {
		return rest, true
	}
	return "", false
}

// buildModuleRelSymbolMap groups batch symbol names by their module-relative
// package directory (see moduleRelDir), the key space cross-package resolution
// uses. Last writer wins on name conflict — illegal within one Go package.
func buildModuleRelSymbolMap(batch application.PromotionBatch, root string) map[string]map[string]domain.NodeID {
	out := make(map[string]map[string]domain.NodeID)
	for _, file := range batch.Files {
		dir := moduleRelDir(file.Path, root)
		bucket, ok := out[dir]
		if !ok {
			bucket = make(map[string]domain.NodeID)
			out[dir] = bucket
		}
		for _, n := range file.Nodes {
			if n != nil {
				bucket[n.Name] = n.ID
			}
		}
	}
	return out
}

// findInBatchMethod walks the per-pkg-dir bucket looking for any method
// whose bare name (the suffix after "Receiver.") equals methodName.
// Returns ("", false) on no match; returns ("", true) [empty id, found=true]
// on ambiguity (multiple receiver types own a method with that name).
// solov2-9rc2: lets the promotion-time resolver bind chained-selector
// calls like `v := pkg.New(...); v.Method()` to the method in pkg, where
// the receiver type is unknown to the parser.
func findInBatchMethod(byPkgDir map[string]map[string]domain.NodeID, relDir, methodName string) (domain.NodeID, bool) {
	bucket, ok := byPkgDir[relDir]
	if !ok {
		return "", false
	}
	suffix := "." + methodName
	var match domain.NodeID
	count := 0
	for name, id := range bucket {
		if strings.HasSuffix(name, suffix) {
			match = id
			count++
		}
	}
	if count == 1 {
		return match, true
	}
	return "", false
}

// lookupPromotedMethodInDir is lookupPromotedSymbolDir's method-by-bare-name
// variant: given a method name like "Hello" and a target package dir, find
// the unique promoted method node whose symbol_path ends with ".Hello" and
// whose file lives in relDir. Returns found=false on miss or on ambiguity
// (multiple receiver types own a Hello method in the same package — rare in
// well-typed Go but possible). solov2-9rc2.
func lookupPromotedMethodInDir(ctx context.Context, tx *sql.Tx, repoID, branch, root, relDir, methodName string) (domain.NodeID, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, file_path FROM nodes
		   WHERE repo_id = ? AND branch = ? AND kind = 'method' AND symbol_path LIKE ?`,
		repoID, branch, "%."+methodName,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	// solov2-9rc2: prefer non-test candidates. Test files commonly declare
	// stub implementations of an interface ("type stubX struct {}; func
	// (s *stubX) Write(...) ...") that share a method name with the
	// production type. If a production match exists, return it without
	// failing on the test-vs-production ambiguity; only when production
	// matches are themselves ambiguous (or absent) do we count test
	// matches in the disambiguation pass.
	var prodMatch, testMatch domain.NodeID
	prodCount, testCount := 0, 0
	for rows.Next() {
		var nodeID, filePath string
		if err := rows.Scan(&nodeID, &filePath); err != nil {
			return "", false, err
		}
		if moduleRelDir(filePath, root) != relDir {
			continue
		}
		if strings.HasSuffix(filePath, "_test.go") {
			testMatch = domain.NodeID(nodeID)
			testCount++
			continue
		}
		prodMatch = domain.NodeID(nodeID)
		prodCount++
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	switch {
	case prodCount == 1:
		return prodMatch, true, nil
	case prodCount == 0 && testCount == 1:
		return testMatch, true, nil
	}
	return "", false, nil
}

// lookupPromotedSymbolDir finds the already-promoted node for symbol `name`
// living in module-relative package dir `relDir`. It scans candidates by
// symbol_path (indexed) and disambiguates by directory in Go, since promoted
// file paths may be absolute or repo-relative. The cursor is fully drained
// before returning so callers may safely write on the same tx afterwards
// (solov2-xc51). found is false on no match.
func lookupPromotedSymbolDir(ctx context.Context, tx *sql.Tx, repoID, branch, root, relDir, name string) (domain.NodeID, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, file_path FROM nodes
		   WHERE repo_id = ? AND branch = ? AND symbol_path = ?`,
		repoID, branch, name,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	var match domain.NodeID
	found := false
	for rows.Next() {
		var nodeID, filePath string
		if err := rows.Scan(&nodeID, &filePath); err != nil {
			return "", false, err
		}
		if found {
			continue // drain remaining rows; ambiguity handled below
		}
		if moduleRelDir(filePath, root) == relDir {
			match = domain.NodeID(nodeID)
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return match, found, nil
}

// isExternalModulePath reports whether importPath looks like a third-party Go
// module (its first segment contains a "." — a hostname like github.com),
// rather than a standard-library package (fmt, net/http). Only external
// modules can match another registered repo, so stdlib calls get no stub.
func isExternalModulePath(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return strings.Contains(first, ".")
}

// stubID derives a deterministic id for a cross-repo edge stub from its source
// node, target module path and symbol, so re-promoting the same call is a
// no-op under the ON CONFLICT clause.
func stubID(srcNodeID, modulePath, symbol string) string {
	h := sha256.Sum256([]byte(srcNodeID + "\x00" + modulePath + "\x00" + symbol))
	return hex.EncodeToString(h[:])
}

// Compile-time assertion that PromotionStore satisfies the application port.
var _ application.PromotionStore = (*PromotionStore)(nil)

// PromotionStore is the SQLite adapter for the application.PromotionStore port.
// It owns the entire promotion transaction: registration check, BEGIN
// IMMEDIATE serializable tx, per-file node delete + re-insert, the registered
// co-transactional PromotionSinks, the post_promotion_queue inserts, and the
// commit. Any error rolls the whole transaction back.
//
// PromotionSinks (FTS, embedding-refs, and any future co-transactional writer)
// are registered at construction time, so adding a sink does not require
// editing the transaction body — the store is open for extension, closed for
// modification.
type PromotionStore struct {
	writeDB   *sql.DB
	sinks     []PromotionSink
	workKinds []string
}

// PromotionStoreOption configures a PromotionStore at construction time.
type PromotionStoreOption func(*PromotionStore)

// WithReviewEnabled gates the optional WorkKindReview lane. When enabled, the
// store enqueues a per-file 'review' queue row in addition to the always-on
// post-promotion kinds; when disabled (the default) no review row is enqueued.
func WithReviewEnabled(enabled bool) PromotionStoreOption {
	return func(s *PromotionStore) {
		s.workKinds = application.PromotionWorkKinds(enabled)
	}
}

// NewPromotionStore constructs a PromotionStore over the write-capable DB
// handle and the given co-transactional sinks. Sinks run in registration order
// inside the promotion transaction.
func NewPromotionStore(writeDB *sql.DB, sinks []PromotionSink, opts ...PromotionStoreOption) *PromotionStore {
	s := &PromotionStore{
		writeDB:   writeDB,
		sinks:     sinks,
		workKinds: application.PromotionWorkKinds(false),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Promote writes the batch in a single atomic transaction. It returns
// application.ErrUnregisteredRepo when the batch's repo is not registered.
func (s *PromotionStore) Promote(ctx context.Context, batch application.PromotionBatch) error {
	rootPath, modulePath, err := s.lookupRepo(ctx, batch.RepoID)
	if err != nil {
		return err
	}

	// An empty batch confirms registration but opens no transaction — there is
	// nothing to write.
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

	// Each phase writes through the shared tx and prepared statements; any
	// error rolls the whole transaction back. Order matters: cross-file edges
	// require every file's nodes to already exist, so edge resolution runs
	// after the per-file node loop.
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

// lookupRepo rejects promotions for repos not in the registry and returns the
// repo's working-tree root and go-module path — both feed cross-package CALLS
// resolution (solov2-xc51). module_path may be NULL/empty, in which case the
// returned module string is "".
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

// promotion carries the state for a single Promote transaction: the open tx,
// the statements prepared once and reused across phases, and the batch-scoped
// scalars. Promote delegates to its phase methods so each stays small and the
// shared statements live in one place instead of threading through arguments.
type promotion struct {
	s          *PromotionStore
	tx         *sql.Tx
	batch      application.PromotionBatch
	repoID     string
	branch     string
	now        int64
	rootPath   string
	modulePath string // "" when the repo has no go-module path

	del        *sql.Stmt
	ins        *sql.Stmt
	prevSigSel *sql.Stmt
	queue      *sql.Stmt
	delImports *sql.Stmt
	insImports *sql.Stmt
	edge       *sql.Stmt
}

// newPromotion prepares the per-transaction statements and primes the
// co-transactional sinks. The caller must defer closeStmts and roll the tx
// back on error.
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

// prepare compiles one statement against the tx, wrapping failures with the
// caller-supplied label to match the original per-statement error messages.
func prepare(ctx context.Context, tx *sql.Tx, label, query string) (*sql.Stmt, error) {
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("promoter: prepare %s: %w", label, err)
	}
	return stmt, nil
}

// prepareStmts compiles the statements reused across the promotion phases. The
// prev-sig select snapshots prior signatures BEFORE the per-file DELETE so the
// re-inserted rows can carry prev_signature forward (the contract-drift check).
// file_imports follows the same DELETE+INSERT lifecycle as nodes (solov2-xjm5).
func (p *promotion) prepareStmts(ctx context.Context) error {
	var err error
	if p.del, err = prepare(ctx, p.tx, "delete",
		`DELETE FROM nodes WHERE file_path = ? AND branch = ? AND repo_id = ?`); err != nil {
		return err
	}
	if p.ins, err = prepare(ctx, p.tx, "insert", `
		INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
			 signature, snippet, prev_signature, exported)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`); err != nil {
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
	// Edge insert is executed in later phases but prepared up front so all
	// reusable statements share one lifecycle. INSERT OR IGNORE matches the
	// autolink path's idempotency — re-promoting the same content is a no-op.
	if p.edge, err = prepare(ctx, p.tx, "edge insert", `
		INSERT INTO edges
			(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at, src_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(edge_id, branch) DO NOTHING`); err != nil {
		return err
	}
	return nil
}

// closeStmts closes every statement prepareStmts opened. Safe to call when
// preparation failed partway: nil statements are skipped.
func (p *promotion) closeStmts() {
	for _, st := range []*sql.Stmt{p.del, p.ins, p.prevSigSel, p.queue, p.delImports, p.insImports, p.edge} {
		if st != nil {
			_ = st.Close()
		}
	}
}

// insertEdge writes one edge through the shared prepared statement. Callers
// wrap the returned error with the phase-specific context they need.
func (p *promotion) insertEdge(ctx context.Context, e *domain.Edge) error {
	_, err := p.edge.ExecContext(ctx,
		e.ID, p.branch, p.repoID,
		string(e.Src), string(e.Tgt),
		string(e.Kind), confidenceText(e.Confidence), p.now,
		edgeSrcLine(e),
	)
	return err
}

// buildCallEdge constructs a Probable CALLS edge for a resolved call site,
// carrying the source line when known. Returns ok=false when the domain
// constructor rejects the inputs, in which case the caller skips the edge.
func buildCallEdge(uc domain.UnresolvedCall, targetID domain.NodeID) (*domain.Edge, bool) {
	opts := []domain.EdgeOption{domain.WithConfidence(domain.Probable)}
	if uc.SrcLine > 0 {
		opts = append(opts, domain.WithSourceLine(uc.SrcLine))
	}
	e, err := domain.NewEdge(uc.CallerID, targetID, domain.EdgeCalls, opts...)
	if err != nil {
		return nil, false
	}
	return e, true
}

// promoteFiles re-promotes every file in the batch: prior-signature snapshot,
// sink pre-delete hooks, node delete + re-insert, import sync, and per-file
// work enqueue.
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
	// Sink pre-delete hooks run while the old node rows still exist — e.g. the
	// FTS sink's node_id IN (SELECT ... FROM nodes ...) deletes MUST resolve
	// against the pre-DELETE rows.
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

// capturePrevSignatures snapshots prior signatures keyed by node_id BEFORE the
// DELETE clears them, so re-inserted rows can thread prev_signature forward. A
// NULL prior signature maps to a nil pointer (meaning "no prior signature
// known") rather than "" so it never falsely registers as a drift.
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

// syncFileImports re-DELETE+INSERTs the file's external imports so removed
// imports disappear in the same commit (solov2-xjm5). Stdlib is skipped to
// mirror the stub-side filter — deps list is for external deps only.
func (p *promotion) syncFileImports(ctx context.Context, file application.PromotionFile) error {
	if _, err := p.delImports.ExecContext(ctx, p.repoID, p.branch, file.Path); err != nil {
		return fmt.Errorf("promoter: delete file_imports for %q: %w", file.Path, err)
	}
	for alias, importPath := range file.Imports {
		if importPath == "" || !isExternalModulePath(importPath) {
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

// insertNode upserts one node and runs the per-node co-transactional sink
// writes (FTS, embedding-refs). prev_signature is NULL when there was no prior
// row for this node_id in (file, branch) — first-time promotions cannot drift.
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
		p.now,
		p.batch.Actor.ID,
		string(p.batch.Actor.Kind),
		nodeSignature(n),
		nodeSnippet(n), // solov2-sxa: bind the capped RawContent so embed-text
		// picks up the body via FetchPending's join.
		prev,
		nodeExported(n),
	); err != nil {
		// Include kind+name+path+lines: a UNIQUE-PK violation here means the
		// parser emitted two nodes with the same (repoID, path, kind, name)
		// tuple, and the bare ID isn't enough to find which symbol
		// (solov2-14lw was diagnosed via these fields).
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

// enqueueWiki enqueues exactly one repo-scoped wiki row per promotion (not
// per-file): the wiki lane regenerates the whole hot_zone + entry_points
// surfaces, so the payload is empty and a single row suffices.
func (p *promotion) enqueueWiki(ctx context.Context) error {
	if _, err := p.queue.ExecContext(ctx,
		p.batch.GitSHA, p.repoID, p.branch, p.batch.GitSHA, string(ports.WorkKindWiki), "", p.now,
	); err != nil {
		return fmt.Errorf("promoter: enqueue wiki: %w", err)
	}
	return nil
}

// insertParserEdges persists parser-produced edges (CALLS, IMPORTS, etc.)
// atomically with the node writes (solov2-ijg). Autolink SIMILAR_TO edges
// arrive separately via the post-promotion queue and don't conflict here.
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

// resolveIntraPackageCalls binds UnresolvedCalls whose callee lives in another
// file of the same Go package (solov2-2at). Package-qualified calls are left to
// the cross-package pass; same-directory = same package by convention. Misses
// stay unresolved.
func (p *promotion) resolveIntraPackageCalls(ctx context.Context) error {
	pkgMaps := buildPackageSymbolMap(p.batch)
	for _, file := range p.batch.Files {
		if len(file.UnresolvedCalls) == 0 {
			continue
		}
		names := pkgMaps[filepath.Dir(file.Path)]
		if len(names) == 0 {
			continue
		}
		for _, uc := range file.UnresolvedCalls {
			// Package-qualified calls (cmd.Execute) bind via the import map in
			// the cross-package pass, never by bare name against the local
			// package — else a same-named local symbol would bind falsely.
			if uc.PkgQualifier != "" {
				continue
			}
			targetID, ok := names[uc.CalleeName]
			if !ok || uc.CallerID == targetID { // miss or self-call (recursion)
				continue
			}
			e, ok := buildCallEdge(uc, targetID)
			if !ok {
				continue
			}
			if err := p.insertEdge(ctx, e); err != nil {
				return fmt.Errorf("promoter: insert cross-file edge %q: %w", e.ID, err)
			}
		}
	}
	return nil
}

// resolveCrossPackageCalls binds package-qualified calls within the same Go
// module and records cross-repo edge stubs for calls into other modules
// (solov2-xc51). Repos without a module_path skip the table entirely.
// Ambiguity/misses are skipped: this pass never emits a false edge.
func (p *promotion) resolveCrossPackageCalls(ctx context.Context) error {
	if p.modulePath == "" {
		return nil
	}
	// Stub statement prepared lazily here so promotions for repos without a
	// module_path never touch the table. Bound by the query-time resolver to
	// whichever registered repo owns the module_path (solov2-xc51.3 / 1gj).
	stubStmt, err := prepare(ctx, p.tx, "stub insert", `
		INSERT INTO cross_repo_edge_stubs
			(stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language, last_promoted_at, method_call, src_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stub_id, branch) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stubStmt.Close()

	byPkgDir := buildModuleRelSymbolMap(p.batch, p.rootPath)
	for _, file := range p.batch.Files {
		if len(file.UnresolvedCalls) == 0 || len(file.Imports) == 0 {
			continue
		}
		for _, uc := range file.UnresolvedCalls {
			if uc.PkgQualifier == "" {
				continue
			}
			if err := p.resolveQualifiedCall(ctx, stubStmt, byPkgDir, file, uc); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveQualifiedCall resolves the call's package qualifier to an import path
// (with the package-name fallback of solov2-izh6.6), then either binds an
// in-module CALLS edge or records a cross-repo stub.
func (p *promotion) resolveQualifiedCall(ctx context.Context, stubStmt *sql.Stmt, byPkgDir map[string]map[string]domain.NodeID, file application.PromotionFile, uc domain.UnresolvedCall) error {
	importPath, ok := file.Imports[uc.PkgQualifier]
	if !ok {
		// The import-alias key is the last URL segment (Go convention), but a
		// module's package name can diverge from its URL (e.g.
		// github.com/jrose/greetlib declares `package greet`). Fall back to a
		// registered Go module among this file's imports whose promoted package
		// node is named uc.PkgQualifier; a single match binds (solov2-izh6.6).
		ip, matched, err := findImportByPackageName(ctx, p.tx, file.Imports, uc.PkgQualifier)
		if err != nil {
			return fmt.Errorf("promoter: cross-repo pkg-name lookup %q: %w", uc.PkgQualifier, err)
		}
		if !matched {
			return nil // qualifier is a local var, an unregistered dep, or stdlib
		}
		importPath = ip
	}
	relDir, inModule := modulePackageDir(p.modulePath, importPath)
	if !inModule {
		return p.emitCrossRepoStub(ctx, stubStmt, uc, importPath)
	}
	return p.resolveInModuleCall(ctx, byPkgDir, uc, relDir)
}

// emitCrossRepoStub records a cross-repo edge stub for a package-qualified call
// into another module. Stdlib (no domain in the first path segment) can never
// match a repo, so it is skipped to keep the table lean. solov2-9rc2 Phase C:
// plain and method calls both emit stubs, distinguished by method_call.
func (p *promotion) emitCrossRepoStub(ctx context.Context, stubStmt *sql.Stmt, uc domain.UnresolvedCall, importPath string) error {
	if !isExternalModulePath(importPath) {
		return nil
	}
	methodFlag := 0
	sid := stubID(string(uc.CallerID), importPath, uc.CalleeName)
	if uc.IsMethodCall {
		methodFlag = 1
		// Distinct stub_id namespace so a same-name plain and method call from
		// the same caller don't collide on the ON CONFLICT key.
		sid = stubID(string(uc.CallerID), importPath, "@method:"+uc.CalleeName)
	}
	if _, err := stubStmt.ExecContext(ctx,
		sid, p.branch, p.repoID, string(uc.CallerID), string(domain.EdgeCalls),
		importPath, uc.CalleeName, "go", p.now, methodFlag,
		ucSrcLine(uc),
	); err != nil {
		return fmt.Errorf("promoter: insert cross-repo stub %q: %w", sid, err)
	}
	return nil
}

// resolveInModuleCall binds an in-module package-qualified call to its target
// node and writes the CALLS edge. Self-calls and misses are skipped.
func (p *promotion) resolveInModuleCall(ctx context.Context, byPkgDir map[string]map[string]domain.NodeID, uc domain.UnresolvedCall, relDir string) error {
	targetID, found, err := p.lookupInModuleTarget(ctx, byPkgDir, relDir, uc)
	if err != nil {
		return err
	}
	if !found || uc.CallerID == targetID {
		return nil
	}
	e, ok := buildCallEdge(uc, targetID)
	if !ok {
		return nil
	}
	if err := p.insertEdge(ctx, e); err != nil {
		return fmt.Errorf("promoter: insert cross-package edge %q: %w", e.ID, err)
	}
	return nil
}

// lookupInModuleTarget finds the target node for an in-module call, first in
// the current batch then in the already-promoted graph (so incremental
// single-file commits still bind). Method calls (solov2-9rc2 Phase B) carry the
// bare method name and match by `<Receiver>.<name>` suffix; single-match binds,
// ambiguity is skipped to preserve the "no false edges" invariant.
func (p *promotion) lookupInModuleTarget(ctx context.Context, byPkgDir map[string]map[string]domain.NodeID, relDir string, uc domain.UnresolvedCall) (domain.NodeID, bool, error) {
	if uc.IsMethodCall {
		if tid, ok := findInBatchMethod(byPkgDir, relDir, uc.CalleeName); ok {
			return tid, true, nil
		}
		tid, found, err := lookupPromotedMethodInDir(ctx, p.tx, p.repoID, p.branch, p.rootPath, relDir, uc.CalleeName)
		if err != nil {
			return "", false, fmt.Errorf("promoter: method-call lookup %q: %w", uc.CalleeName, err)
		}
		return tid, found, nil
	}
	if tid, ok := byPkgDir[relDir][uc.CalleeName]; ok {
		return tid, true, nil
	}
	// Fall back to the promoted graph (callee's file not in this batch). The
	// cursor must fully drain before the edge insert: a query open during
	// ExecContext deadlocks the single write connection.
	tid, found, err := lookupPromotedSymbolDir(ctx, p.tx, p.repoID, p.branch, p.rootPath, relDir, uc.CalleeName)
	if err != nil {
		return "", false, fmt.Errorf("promoter: cross-package lookup %q: %w", uc.CalleeName, err)
	}
	return tid, found, nil
}

// advanceRepoSHA advances repos.last_promoted_sha (and active_branch when a
// branch is supplied) atomically with the node writes (solov2-c47). An empty
// SHA is treated as caller error and skipped so a known-good value is not
// clobbered; an empty branch (repo.Add does not set active_branch) writes the
// SHA alone and leaves active_branch untouched.
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

// findImportByPackageName looks for a registered Go module among the
// supplied imports whose promoted package node is named pkgName. Used as
// a fallback when the parser's import-alias key (last URL segment) does
// not match the call-site qualifier — common when a module's package
// declaration diverges from its URL (e.g. github.com/jrose/greetlib
// declares `package greet`). solov2-izh6.6.
//
// Returns the matching import path when exactly one registered module
// matches. Multiple matches are reported as "no match" rather than
// guessing — emitting a stub against the wrong module would violate the
// promoter's "no false edges" invariant; ambiguity is a real signal to
// fix the call site or add an explicit import alias upstream.
func findImportByPackageName(ctx context.Context, tx *sql.Tx, imports map[string]string, pkgName string) (string, bool, error) {
	if pkgName == "" || len(imports) == 0 {
		return "", false, nil
	}
	// Collect candidate module paths — only those that look external
	// (have a '.' in the first segment); stdlib never reverse-resolves to
	// a registered repo and we want to keep the IN-list short.
	paths := make([]string, 0, len(imports))
	args := []any{pkgName}
	for _, p := range imports {
		if !isExternalModulePath(p) {
			continue
		}
		paths = append(paths, p)
		args = append(args, p)
	}
	if len(paths) == 0 {
		return "", false, nil
	}
	placeholders := strings.Repeat("?,", len(paths))
	placeholders = placeholders[:len(placeholders)-1]
	q := `
		SELECT DISTINCT r.module_path
		FROM nodes n
		JOIN repos r ON r.repo_id = n.repo_id
		WHERE n.kind = 'package' AND n.symbol_path = ?
		  AND r.module_path IN (` + placeholders + `)`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return "", false, fmt.Errorf("find import by pkg name: %w", err)
	}
	defer rows.Close()
	var match string
	count := 0
	for rows.Next() {
		var mp string
		if scanErr := rows.Scan(&mp); scanErr != nil {
			return "", false, fmt.Errorf("find import by pkg name: scan: %w", scanErr)
		}
		match = mp
		count++
		if count > 1 {
			return "", false, nil // ambiguous — caller will skip
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("find import by pkg name: iter: %w", err)
	}
	if count == 1 {
		return match, true, nil
	}
	return "", false, nil
}

// nodeLanguage returns the language string or "" when not set.
func nodeLanguage(n *domain.Node) string {
	if n.Language == nil {
		return ""
	}
	return *n.Language
}

// nodeLines returns (lineStart, lineEnd) as values so NULL is written when the
// node has no line range.
func nodeLines(n *domain.Node) (lineStart, lineEnd any) {
	if n.Lines == nil {
		return nil, nil
	}
	return n.Lines.Start, n.Lines.End
}

// nodeContentHash returns the content hash string or "" when not set.
func nodeContentHash(n *domain.Node) string {
	if n.ContentHash == nil {
		return ""
	}
	return string(*n.ContentHash)
}

// nodeSignature returns the signature string for the INSERT bind, or nil so
// SQLite writes a NULL when the parser did not populate it. Returning the
// empty string here would conflate "unknown signature" with "known empty
// signature" and break the contract-drift comparison.
func nodeSignature(n *domain.Node) any {
	if n.Signature == nil {
		return nil
	}
	return *n.Signature
}

// nodeExported returns 1/0 for the INSERT bind, or nil so SQLite writes NULL
// when the parser did not set the flag — keeping "unknown" (e.g. a language
// with no export concept) distinct from "known unexported".
func nodeExported(n *domain.Node) any {
	if n.Exported == nil {
		return nil
	}
	if *n.Exported {
		return 1
	}
	return 0
}
