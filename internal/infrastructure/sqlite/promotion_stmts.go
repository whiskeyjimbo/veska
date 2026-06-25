// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, branch) DO NOTHING`); err != nil {
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
			(repo_id, branch, file_path, import_path, alias, language, last_promoted_at, internal)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
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
