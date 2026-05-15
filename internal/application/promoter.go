package application

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"github.com/whiskeyjimbo/veska/internal/tokenize"
	"go.opentelemetry.io/otel/trace"
)

// workKinds lists the post-promotion work kinds enqueued per file.
var workKinds = []string{
	string(ports.WorkKindEmbed),
	string(ports.WorkKindAutoLink),
	string(ports.WorkKindRevalidate),
}

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
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
			 signature, prev_signature)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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

	// FTS write path (m3.03.2): two virtual tables — node_fts_words holds
	// the pre-tokenised camelCase/snake_case/`.`/`::`-split form for
	// unicode61, node_fts_trigrams holds the raw concatenated text for
	// FTS5's built-in trigram tokenizer. Both must be kept atomic with
	// the parent node row, which is why the inserts live inside the
	// promotion tx. Deletes are by (node_id, branch, repo_id) on each
	// FTS table — FTS5 allows DELETE WHERE-clauses against UNINDEXED
	// columns even though they're not indexed.
	delWordsStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM node_fts_words WHERE node_id = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare delete fts_words: %w", err)
	}
	defer delWordsStmt.Close()

	delTrigramsStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM node_fts_trigrams WHERE node_id = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare delete fts_trigrams: %w", err)
	}
	defer delTrigramsStmt.Close()

	insWordsStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert fts_words: %w", err)
	}
	defer insWordsStmt.Close()

	insTrigramsStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert fts_trigrams: %w", err)
	}
	defer insTrigramsStmt.Close()

	// Note on deletes: per-file FTS deletes happen inline below before the
	// `nodes` DELETE (so the subquery sees the still-present rows). The
	// prepared per-node DELETE statements are kept as a defensive idempotency
	// net — a re-promote within the same tx would otherwise duplicate FTS
	// rows for nodes that survive the parse.

	now := time.Now().UnixMilli()

	for filePath, nodes := range snap {
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

		// Delete the FTS rows for every node that currently lives in this
		// file BEFORE deleting the node rows themselves. Doing it via a
		// node_id IN (SELECT ...) keeps the FTS in sync even for nodes
		// that the new parse no longer produces (file shrank / symbol
		// removed). FTS5 allows DELETE WHERE-clauses over UNINDEXED
		// columns; the trade-off is a scan rather than an index probe,
		// which is fine for a per-file write — the per-file row count is
		// tiny relative to the table.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_fts_words
			 WHERE branch = ? AND repo_id = ?
			   AND node_id IN (SELECT node_id FROM nodes
			                   WHERE file_path = ? AND branch = ? AND repo_id = ?)`,
			branch, repoID, filePath, branch, repoID,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete fts_words for file %q: %w", filePath, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_fts_trigrams
			 WHERE branch = ? AND repo_id = ?
			   AND node_id IN (SELECT node_id FROM nodes
			                   WHERE file_path = ? AND branch = ? AND repo_id = ?)`,
			branch, repoID, filePath, branch, repoID,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete fts_trigrams for file %q: %w", filePath, err)
		}

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
				actor.ID,
				string(actor.Kind),
				sig,
				prev,
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

			// FTS write: DELETE any prior row for this node_id, then INSERT.
			// raw = "<kind> <symbol_path>" — what the trigram tokenizer sees.
			// n.Name is the qualified symbol path stored in the `symbol_path`
			// column (the domain conflates "name" and "symbol_path" — see
			// NewNode). The file path is intentionally omitted: it tends to
			// be long, noisy, and orthogonal to symbol-level lookup.
			// words = tokenize.Symbol of the same — what the unicode61
			// tokenizer sees after camelCase/snake_case/`::`/`.` splits.
			rawFTS := string(n.Kind) + " " + n.Name
			wordsFTS := tokenize.Symbol(rawFTS)
			if _, err := delWordsStmt.ExecContext(ctx, string(n.ID), branch, repoID); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: delete fts_words for %q: %w", n.ID, err)
			}
			if _, err := delTrigramsStmt.ExecContext(ctx, string(n.ID), branch, repoID); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: delete fts_trigrams for %q: %w", n.ID, err)
			}
			if _, err := insWordsStmt.ExecContext(ctx, string(n.ID), branch, repoID, wordsFTS); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert fts_words for %q: %w", n.ID, err)
			}
			if _, err := insTrigramsStmt.ExecContext(ctx, string(n.ID), branch, repoID, rawFTS); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert fts_trigrams for %q: %w", n.ID, err)
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
