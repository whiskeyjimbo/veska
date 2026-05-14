package application

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"go.opentelemetry.io/otel/trace"
)

// workKinds lists the post-promotion work kinds enqueued per file.
var workKinds = []string{"embed", "auto_link", "revalidate"}

// ErrUnregisteredRepo is returned by Promote when the repoID is not found in
// the repos table. The daemon must never promote work from an unknown repo.
type ErrUnregisteredRepo struct{ RepoID string }

func (e ErrUnregisteredRepo) Error() string {
	return fmt.Sprintf(
		"promoter: repo %q is not registered — run: veska repo add <path>",
		e.RepoID,
	)
}

// CheckRunInput is the data the post-commit check pipeline receives. It mirrors
// internal/application/checks.Input but is declared here so that Promoter can
// reference the runner without importing the sub-package (preventing an import
// cycle: checks already depends on observability + ports, and application
// imports checks would force application's tests to drag in metrics setup for
// every promoter test).
type CheckRunInput struct {
	RepoID    string
	Branch    string
	GitSHA    string
	FilePaths []string
}

// CheckRunner is the contract Promoter requires from the post-commit
// structural check pipeline. The concrete implementation lives in
// internal/application/checks. CheckRunner.Run is invoked AFTER the promotion
// transaction commits and MUST NOT return an error — findings are advisory and
// cannot abort a promotion.
type CheckRunner interface {
	Run(ctx context.Context, in CheckRunInput)
}

// Promoter flushes the in-memory StagingArea to SQLite in a single atomic
// BEGIN IMMEDIATE transaction and enqueues post-promotion work items.
type Promoter struct {
	staging *StagingArea
	writeDB *sql.DB
	tp      observability.TracerProvider
	audit   ports.AuditWriter
	checks  CheckRunner
}

// NewPromoter constructs a Promoter wired to the provided StagingArea and
// write-only SQLite handle.
func NewPromoter(staging *StagingArea, writeDB *sql.DB) *Promoter {
	return &Promoter{
		staging: staging,
		writeDB: writeDB,
	}
}

// SetAuditWriter installs an AuditWriter for promotion audit entries.
// If not called (or called with nil), audit writes are skipped.
func (p *Promoter) SetAuditWriter(aw ports.AuditWriter) {
	p.audit = aw
}

// SetCheckRunner installs the post-commit structural check runner. The runner
// is invoked after the promotion transaction commits, before Promote returns.
// If not called (or called with nil), no checks run.
func (p *Promoter) SetCheckRunner(r CheckRunner) {
	p.checks = r
}

// SetTracerProvider installs a TracerProvider for promotion.transaction spans.
// If not called (or called with nil), a noop provider is used.
func (p *Promoter) SetTracerProvider(tp observability.TracerProvider) {
	p.tp = tp
}

// tracerProvider returns the configured provider or a noop if nil.
func (p *Promoter) tracerProvider() observability.TracerProvider {
	if p.tp == nil {
		return trace.NewNoopTracerProvider()
	}
	return p.tp
}

// Promote is called by the post-commit hook.  It:
//  1. Takes a snapshot of all nodes staged for (repoID, branch).
//  2. Opens a BEGIN IMMEDIATE transaction.
//  3. For each staged file: deletes existing nodes then upserts the new ones.
//  4. Enqueues one post_promotion_queue row per file per work_kind inside the
//     same transaction.
//  5. Commits — all writes land atomically or not at all.
//  6. Calls StagingArea.DeleteStagedFile for each promoted file after commit.
//
// Node-only promotion: edges are intentionally not promoted here. They are
// re-derived post-promotion by the auto_link queue worker (work_kind="auto_link").
// Staged edges remain in the StagingArea solely to serve pre-promotion overlay reads.
//
// actor records who triggered the promotion. Hook-triggered paths should pass
// domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}.
func (p *Promoter) Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
	// Reject promotions for repos not in the registry.
	var exists int
	err := p.writeDB.QueryRowContext(ctx,
		`SELECT 1 FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrUnregisteredRepo{RepoID: repoID}
	}
	if err != nil {
		return fmt.Errorf("promoter: check repo registration: %w", err)
	}

	snap := p.staging.Snapshot(repoID, branch)
	if len(snap) == 0 {
		return nil
	}

	ctx, span := observability.StartSpan(ctx, p.tracerProvider(), "promotion.transaction")
	defer span.End()

	tx, err := p.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
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
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert: %w", err)
	}
	defer insStmt.Close()

	queueStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare queue insert: %w", err)
	}
	defer queueStmt.Close()

	// Enqueue per-node pending row into node_embedding_refs, committed
	// atomically with the node upsert. The embedder worker (m3.02.1) drains
	// state='pending' rows; here we only record the intent.
	//
	// node_id is the PK: re-promoting the same node resets its embedding
	// state back to pending so the worker will re-embed it. content_hash is
	// NULL until the worker computes the embedding bytes (m3.02 §2).
	embedRefStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO node_embedding_refs
			(node_id, content_hash, state, enqueued_at, embedded_at)
		VALUES (?, NULL, 'pending', ?, NULL)
		ON CONFLICT(node_id) DO UPDATE SET
			content_hash = NULL,
			state        = 'pending',
			enqueued_at  = excluded.enqueued_at,
			embedded_at  = NULL`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare embed ref insert: %w", err)
	}
	defer embedRefStmt.Close()

	now := time.Now().UnixMilli()

	for filePath, nodes := range snap {
		// Delete all existing nodes for this file+branch+repo before re-inserting.
		if _, err := delStmt.ExecContext(ctx, filePath, branch, repoID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete nodes for %q: %w", filePath, err)
		}

		// Upsert all nodes for this file.
		for _, n := range nodes {
			lang := nodeLanguage(n)
			lineStart, lineEnd := nodeLines(n)
			contentHash := nodeContentHash(n)

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
				actor.ID,
				string(actor.Kind),
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert node %q: %w", n.ID, err)
			}

			// Mirror the just-promoted node into node_embedding_refs as
			// state='pending'. Atomic with the node insert by sharing tx.
			if _, err := embedRefStmt.ExecContext(ctx, string(n.ID), now); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: enqueue embed ref for %q: %w", n.ID, err)
			}
		}

		// Enqueue one row per work_kind for this file.
		for _, wk := range workKinds {
			if _, err := queueStmt.ExecContext(ctx,
				gitSHA, repoID, branch, gitSHA, wk, filePath, now,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: enqueue %q for %q: %w", wk, filePath, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("promoter: commit: %w", err)
	}

	// Clear staging entries only after a successful commit.
	promotedAt := time.Now()
	filePaths := make([]string, 0, len(snap))
	for filePath := range snap {
		filePaths = append(filePaths, filePath)
		p.staging.DeleteStagedFile(repoID, branch, filePath)
		if p.audit != nil {
			_ = p.audit.Write(ctx, ports.AuditEntry{
				RepoID:    repoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        "promotion.commit",
				TargetID:  filePath,
				Branch:    branch,
				CreatedAt: promotedAt,
			})
		}
	}

	// Post-commit: run advisory structural checks against the just-committed
	// slice of the graph. Findings cannot abort the promotion — by contract
	// the runner does not return an error.
	if p.checks != nil {
		p.checks.Run(ctx, CheckRunInput{
			RepoID:    repoID,
			Branch:    branch,
			GitSHA:    gitSHA,
			FilePaths: filePaths,
		})
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

// nodeLines returns (lineStart, lineEnd) as sql.NullInt64 pairs so NULL is
// written when the node has no line range.
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
