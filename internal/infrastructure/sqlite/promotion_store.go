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
	repoID := batch.RepoID
	branch := batch.Branch

	// Reject promotions for repos not in the registry. Capture the repo's
	// working-tree root and go-module path here too: both feed cross-package
	// CALLS resolution below (solov2-xc51). module_path may be NULL/empty.
	var rootPath, modulePath sql.NullString
	err := s.writeDB.QueryRowContext(ctx,
		`SELECT root_path, module_path FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&rootPath, &modulePath)
	if err == sql.ErrNoRows {
		return application.ErrUnregisteredRepo{RepoID: repoID}
	}
	if err != nil {
		return fmt.Errorf("promoter: check repo registration: %w", err)
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

	// Prepare statements within the transaction for efficiency.
	delStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM nodes WHERE file_path = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare delete: %w", err)
	}
	defer delStmt.Close()

	insStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
			 signature, snippet, prev_signature, exported)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert: %w", err)
	}
	defer insStmt.Close()

	// Snapshot the prior signature for each (node_id) in (file, branch, repo)
	// BEFORE the per-file DELETE so the new row can carry it forward as
	// prev_signature. This is what powers the contract-drift check without
	// requiring a separate history table.
	prevSigSelectStmt, err := tx.PrepareContext(ctx, `
		SELECT node_id, signature FROM nodes
		WHERE file_path = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare prev-sig select: %w", err)
	}
	defer prevSigSelectStmt.Close()

	queueStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare queue insert: %w", err)
	}
	defer queueStmt.Close()

	// solov2-xjm5: persist parsed imports per file so `veska deps list` can
	// surface modules that are imported but only referenced via struct
	// literals / type assertions (no resolved CALLS edge into stubs). Like
	// the nodes table the rows are scoped to (repo_id, branch, file_path)
	// and re-promotion DELETE+INSERTs so removed imports disappear in the
	// same commit.
	delImportsStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM file_imports WHERE repo_id = ? AND branch = ? AND file_path = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare file_imports delete: %w", err)
	}
	defer delImportsStmt.Close()
	insImportsStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_imports
			(repo_id, branch, file_path, import_path, alias, language, last_promoted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, branch, file_path, import_path) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare file_imports insert: %w", err)
	}
	defer insImportsStmt.Close()

	// Prepare each co-transactional sink once against the tx.
	for _, sink := range s.sinks {
		if err := sink.Prepare(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: prepare sink: %w", err)
		}
	}

	now := batch.PromotedAt

	for _, file := range batch.Files {
		filePath := file.Path

		// Capture prior signatures keyed by node_id BEFORE the DELETE clears
		// them, so we can thread prev_signature into the re-inserted rows.
		// Nodes with NULL signature in the prior row map to a nil pointer so
		// the new row's prev_signature remains NULL — equivalent to "no prior
		// signature known" rather than "" which would falsely register as a
		// drift to/from the empty string.
		prevSig := make(map[string]*string)
		prevRows, err := prevSigSelectStmt.QueryContext(ctx, filePath, branch, repoID)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: select prev signatures for %q: %w", filePath, err)
		}
		for prevRows.Next() {
			var nodeID string
			var sig sql.NullString
			if err := prevRows.Scan(&nodeID, &sig); err != nil {
				_ = prevRows.Close()
				_ = tx.Rollback()
				return fmt.Errorf("promoter: scan prev signature for %q: %w", filePath, err)
			}
			if sig.Valid {
				v := sig.String
				prevSig[nodeID] = &v
			} else {
				prevSig[nodeID] = nil
			}
		}
		if err := prevRows.Err(); err != nil {
			_ = prevRows.Close()
			_ = tx.Rollback()
			return fmt.Errorf("promoter: iterate prev signatures for %q: %w", filePath, err)
		}
		if err := prevRows.Close(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: close prev signatures for %q: %w", filePath, err)
		}

		// Sink pre-delete hooks run while the old node rows still exist — e.g.
		// the FTS sink's node_id IN (SELECT ... FROM nodes ...) deletes MUST
		// resolve against the pre-DELETE rows.
		for _, sink := range s.sinks {
			if err := sink.BeforeNodeDelete(ctx, tx, branch, repoID, filePath); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: sink before-delete for %q: %w", filePath, err)
			}
		}

		// Delete all existing nodes for this file+branch+repo before re-inserting.
		if _, err := delStmt.ExecContext(ctx, filePath, branch, repoID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete nodes for %q: %w", filePath, err)
		}
		// solov2-xjm5: same DELETE+re-INSERT lifecycle for file_imports.
		if _, err := delImportsStmt.ExecContext(ctx, repoID, branch, filePath); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete file_imports for %q: %w", filePath, err)
		}
		for alias, importPath := range file.Imports {
			if importPath == "" {
				continue
			}
			// Skip stdlib (no domain in the first path segment) to mirror
			// the stub-side filter — deps list is for external deps, and
			// surfacing every stdlib package would drown the output
			// (solov2-xjm5).
			if !isExternalModulePath(importPath) {
				continue
			}
			if _, err := insImportsStmt.ExecContext(ctx,
				repoID, branch, filePath, importPath, alias, "go", now,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert file_imports for %q (%s): %w", filePath, importPath, err)
			}
		}

		// Upsert all nodes for this file.
		for _, n := range file.Nodes {
			lang := nodeLanguage(n)
			lineStart, lineEnd := nodeLines(n)
			contentHash := nodeContentHash(n)
			sig := nodeSignature(n)
			// prev signature: NULL when there was no prior row for this node_id
			// in (file, branch) — first-time promotions cannot drift.
			var prev any
			if ps, ok := prevSig[string(n.ID)]; ok && ps != nil {
				prev = *ps
			} else {
				prev = nil
			}

			if _, err := insStmt.ExecContext(ctx,
				string(n.ID),
				branch,
				repoID,
				lang,
				string(n.Kind),
				n.Name,
				n.Path,
				lineStart,
				lineEnd,
				contentHash,
				now,
				batch.Actor.ID,
				string(batch.Actor.Kind),
				sig,
				nodeSnippet(n), // solov2-sxa: bind the capped RawContent so
				// embed-text picks up the body via FetchPending's join.
				prev,
				nodeExported(n),
			); err != nil {
				_ = tx.Rollback()
				// Include kind+name+path+lines: a UNIQUE-PK violation here means
				// the parser emitted two nodes with the same (repoID, path,
				// kind, name) tuple, and the bare ID isn't enough to find
				// which symbol — solov2-14lw was diagnosed via these fields.
				return fmt.Errorf("promoter: insert node %q (kind=%s name=%q path=%q lines=%v): %w",
					n.ID, n.Kind, n.Name, n.Path, n.Lines, err)
			}

			// Per-node co-transactional sink writes (FTS, embedding-refs).
			nw := nodeWrite{
				NodeID: string(n.ID),
				Branch: branch,
				RepoID: repoID,
				Kind:   string(n.Kind),
				Symbol: n.Name,
			}
			for _, sink := range s.sinks {
				if err := sink.AfterNodeInsert(ctx, tx, nw, now); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("promoter: sink after-insert for %q: %w", n.ID, err)
				}
			}
		}

		// Enqueue one row per work_kind for this file.
		for _, wk := range s.workKinds {
			if _, err := queueStmt.ExecContext(ctx,
				batch.GitSHA, repoID, branch, batch.GitSHA, wk, filePath, now,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: enqueue %q for %q: %w", wk, filePath, err)
			}
		}
	}

	// Enqueue exactly one repo-scoped WorkKindWiki row per promotion (not
	// per-file). The wiki lane regenerates the whole hot_zone + entry_points
	// surfaces, so a single row per promotion is sufficient; the payload is
	// empty because the handler operates on repo-scoped state.
	if _, err := queueStmt.ExecContext(ctx,
		batch.GitSHA, repoID, branch, batch.GitSHA, string(ports.WorkKindWiki), "", now,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: enqueue wiki: %w", err)
	}

	// Persist parser-produced edges (CALLS, IMPORTS, etc.) atomically with
	// the node writes (solov2-ijg). Cross-file edges (e.g. main.go's
	// NewServer → store.go's NewNoteStore) require both files' nodes to
	// exist in the table, so the edge insert runs AFTER the per-file node
	// loop completes. INSERT OR IGNORE matches the autolink path's
	// idempotency — re-promoting the same content is a no-op.
	//
	// Autolink-produced SIMILAR_TO edges still arrive separately via the
	// post-promotion queue; they don't conflict with this insert (different
	// edge_id by construction).
	edgeStmt, eerr := tx.PrepareContext(ctx, `
		INSERT INTO edges
			(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(edge_id, branch) DO NOTHING`)
	if eerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare edge insert: %w", eerr)
	}
	defer edgeStmt.Close()

	for _, file := range batch.Files {
		for _, e := range file.Edges {
			if e == nil {
				continue
			}
			if _, ierr := edgeStmt.ExecContext(ctx,
				e.ID, branch, repoID,
				string(e.Src), string(e.Tgt),
				string(e.Kind), confidenceText(e.Confidence), now,
			); ierr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert edge %q: %w", e.ID, ierr)
			}
		}
	}

	// Cross-file intra-package CALLS resolution (solov2-2at). The parser
	// emits UnresolvedCalls when a call site names a symbol absent from
	// the file's own symbol map; typically the callee lives in another
	// file of the same Go package (foo.go calling NewBar() defined in
	// bar.go). Build a per-directory map of name → node_id from the
	// batch and resolve. Same-directory = same Go package by
	// convention. Misses (e.g. cross-package, stdlib) stay unresolved.
	//
	// CALLS edges from this pass are written through the same prepared
	// stmt as parser-produced edges so confidence + idempotency match.
	pkgMaps := buildPackageSymbolMap(batch)
	for _, file := range batch.Files {
		if len(file.UnresolvedCalls) == 0 {
			continue
		}
		pkgKey := filepath.Dir(file.Path)
		names := pkgMaps[pkgKey]
		if len(names) == 0 {
			continue
		}
		for _, uc := range file.UnresolvedCalls {
			// Package-qualified calls (cmd.Execute) are resolved by the
			// cross-package pass via the import map, never by bare name
			// against the local package — otherwise a same-named symbol in
			// the caller's package would bind falsely (solov2-xc51).
			if uc.PkgQualifier != "" {
				continue
			}
			targetID, ok := names[uc.CalleeName]
			if !ok {
				continue
			}
			if uc.CallerID == targetID {
				// Self-call (recursion) — skip.
				continue
			}
			e, eerr := domain.NewEdge(uc.CallerID, targetID, domain.EdgeCalls,
				domain.WithConfidence(domain.Probable),
			)
			if eerr != nil {
				continue
			}
			if _, ierr := edgeStmt.ExecContext(ctx,
				e.ID, branch, repoID,
				string(e.Src), string(e.Tgt),
				string(e.Kind), confidenceText(e.Confidence), now,
			); ierr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert cross-file edge %q: %w", e.ID, ierr)
			}
		}
	}

	// Cross-package CALLS resolution within the same Go module (solov2-xc51).
	// A package-qualified call (cmd.Execute) binds by resolving the import
	// alias to a package directory under this repo's module, then matching the
	// callee name to a node in that package — first in the current batch, then
	// in the already-promoted graph so incremental single-file commits still
	// bind. Imports outside this module fall through to xc51.3 (cross-repo
	// stubs). Ambiguity/misses are skipped: this pass never emits a false edge.
	if modulePath.Valid && modulePath.String != "" {
		mod := modulePath.String
		root := rootPath.String
		byPkgDir := buildModuleRelSymbolMap(batch, root)

		// Cross-repo edge stubs for package-qualified calls into other modules
		// (solov2-xc51.3 / solov2-1gj). Prepared lazily here so promotions for
		// repos without a module_path never touch the table. The query-time
		// resolver binds these to a node in whatever registered repo owns the
		// module_path. Idempotent on the deterministic stub_id.
		stubStmt, serr := tx.PrepareContext(ctx, `
			INSERT INTO cross_repo_edge_stubs
				(stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language, last_promoted_at, method_call)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(stub_id, branch) DO NOTHING`)
		if serr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: prepare stub insert: %w", serr)
		}
		defer stubStmt.Close()
		for _, file := range batch.Files {
			if len(file.UnresolvedCalls) == 0 || len(file.Imports) == 0 {
				continue
			}
			for _, uc := range file.UnresolvedCalls {
				if uc.PkgQualifier == "" {
					continue
				}
				importPath, ok := file.Imports[uc.PkgQualifier]
				if !ok {
					continue // qualifier is a local var, not an import
				}
				relDir, inModule := modulePackageDir(mod, importPath)
				if !inModule {
					// Import resolves to another module. Record a cross-repo
					// edge stub the query-time resolver binds to whichever
					// registered repo owns module_path == importPath. Stdlib
					// (no domain in the first path segment) can never match a
					// repo, so it is skipped to keep the table lean.
					//
					// solov2-9rc2 Phase C: both plain pkg.Foo() and method-call
					// uc.IsMethodCall emit cross-repo stubs. The method_call
					// column lets the resolver branch on the lookup strategy —
					// plain stubs match exact symbol_path; method-call stubs
					// match `<Receiver>.<symbol_path>` suffix.
					if isExternalModulePath(importPath) {
						methodFlag := 0
						if uc.IsMethodCall {
							methodFlag = 1
						}
						sid := stubID(string(uc.CallerID), importPath, uc.CalleeName)
						if uc.IsMethodCall {
							// Distinct stub_id namespace so a same-name plain
							// and method call from the same caller don't
							// collide on the ON CONFLICT key (sid is
							// deterministic over caller+pkg+name, which is
							// identical between the two callsite shapes).
							sid = stubID(string(uc.CallerID), importPath, "@method:"+uc.CalleeName)
						}
						if _, ierr := stubStmt.ExecContext(ctx,
							sid, branch, repoID, string(uc.CallerID), string(domain.EdgeCalls),
							importPath, uc.CalleeName, "go", now, methodFlag,
						); ierr != nil {
							_ = tx.Rollback()
							return fmt.Errorf("promoter: insert cross-repo stub %q: %w", sid, ierr)
						}
					}
					continue
				}
				// In-module resolution. solov2-9rc2 Phase B: a method call
				// (uc.IsMethodCall) carries only the bare method name in
				// CalleeName ("Hello" from `v.Hello(...)`), so look it up by
				// suffix match against `<Receiver>.Hello` rather than exact
				// symbol_path. Single-match binds; ambiguity is skipped to
				// preserve the "no false edges" invariant.
				var targetID domain.NodeID
				var inBatch bool
				if uc.IsMethodCall {
					targetID, inBatch = findInBatchMethod(byPkgDir, relDir, uc.CalleeName)
					if !inBatch {
						tid, found, qerr := lookupPromotedMethodInDir(ctx, tx, repoID, branch, root, relDir, uc.CalleeName)
						if qerr != nil {
							_ = tx.Rollback()
							return fmt.Errorf("promoter: method-call lookup %q: %w", uc.CalleeName, qerr)
						}
						if !found {
							continue
						}
						targetID = tid
					}
				} else {
					targetID, inBatch = byPkgDir[relDir][uc.CalleeName]
					if !inBatch {
						// Fall back to the promoted graph (callee's file not in
						// this batch). Must fully drain the cursor before the
						// edge insert: a query open during ExecContext deadlocks
						// the single write connection.
						tid, found, qerr := lookupPromotedSymbolDir(ctx, tx, repoID, branch, root, relDir, uc.CalleeName)
						if qerr != nil {
							_ = tx.Rollback()
							return fmt.Errorf("promoter: cross-package lookup %q: %w", uc.CalleeName, qerr)
						}
						if !found {
							continue
						}
						targetID = tid
					}
				}
				if uc.CallerID == targetID {
					continue
				}
				e, eerr := domain.NewEdge(uc.CallerID, targetID, domain.EdgeCalls,
					domain.WithConfidence(domain.Probable),
				)
				if eerr != nil {
					continue
				}
				if _, ierr := edgeStmt.ExecContext(ctx,
					e.ID, branch, repoID,
					string(e.Src), string(e.Tgt),
					string(e.Kind), confidenceText(e.Confidence), now,
				); ierr != nil {
					_ = tx.Rollback()
					return fmt.Errorf("promoter: insert cross-package edge %q: %w", e.ID, ierr)
				}
			}
		}
	}

	// Advance repos.last_promoted_sha (and repos.active_branch when the
	// caller supplied one) atomically with the node writes. Without this,
	// StartupResync's cheap-path check (LastPromotedSHA == HEAD) has nothing
	// to compare against — every daemon restart treats every repo as
	// never-promoted and re-runs the full reparser (solov2-c47).
	//
	// An empty SHA is treated as caller error and skipped so we don't clobber
	// a known-good value with "". An empty branch is a real production case
	// (repo.Add does not set active_branch), so we write the SHA alone in
	// that case and leave active_branch untouched.
	if batch.GitSHA != "" {
		var execErr error
		if branch != "" {
			_, execErr = tx.ExecContext(ctx,
				`UPDATE repos SET last_promoted_sha = ?, active_branch = ? WHERE repo_id = ?`,
				batch.GitSHA, branch, repoID,
			)
		} else {
			_, execErr = tx.ExecContext(ctx,
				`UPDATE repos SET last_promoted_sha = ? WHERE repo_id = ?`,
				batch.GitSHA, repoID,
			)
		}
		if execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: advance last_promoted_sha: %w", execErr)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("promoter: commit: %w", err)
	}
	return nil
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
