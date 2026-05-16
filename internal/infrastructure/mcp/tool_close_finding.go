package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CodeHumanRequired is the custom JSON-RPC error code returned when a
// severity >= high finding is closed by a non-human actor.
const CodeHumanRequired = -32001

// ---------------------------------------------------------------------------
// eng_close_finding
// ---------------------------------------------------------------------------

type closeFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
}

func makeCloseFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p closeFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "branch", p.Branch, "repo_id", p.RepoID, "reason", p.Reason); rpcErr != nil {
			return nil, rpcErr
		}

		// Open a single transaction so finding-close and any edge-promotion
		// commit or roll back together.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("begin tx: %v", err)}
		}
		// Ensure rollback on any non-commit return path. After a successful
		// Commit, Rollback is a no-op per database/sql.
		defer func() { _ = tx.Rollback() }()

		// Fetch the finding to check existence, severity, rule, and anchor
		// (node_id, which for auto-link findings carries the edge_id).
		var (
			severity string
			state    string
			rule     string
			nodeID   sql.NullString
		)
		err = tx.QueryRowContext(ctx,
			`SELECT severity, state, rule, node_id FROM findings WHERE finding_id = ? AND branch = ?`,
			p.FindingID, p.Branch,
		).Scan(&severity, &state, &rule, &nodeID)
		if err == sql.ErrNoRows {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
		}

		// Human-action gate: severity >= high requires a human actor.
		sev := domain.Severity(severity)
		if sev.AtLeast(domain.SeverityHigh) && actor.Kind != domain.ActorKindHuman {
			return nil, &RPCError{
				Code:    CodeHumanRequired,
				Message: "human_required",
				Data: map[string]any{
					"reason":     "human_required",
					"finding_id": p.FindingID,
					"severity":   severity,
				},
			}
		}

		// Accept-flow for auto-link findings promotes the anchored edge from
		// 'unresolved' to 'definite' in the same tx. Any other (rule, reason)
		// combination is a regular close.
		//
		// Behaviours for edge anchors:
		//   - Missing/empty node_id: skip promotion silently; finding still closes.
		//   - Missing edge row: data-corruption-soft-fail — UPDATE affects 0 rows,
		//     finding still closes (no rollback). Logged via the absence of an
		//     audit follow-up beyond the standard accept entry.
		//   - Already 'definite' edge: UPDATE is naturally idempotent.
		isAccept := rule == "auto-link" && p.Reason == "accept"
		if isAccept && nodeID.Valid && nodeID.String != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE edges SET confidence = 'definite' WHERE edge_id = ? AND branch = ?`,
				nodeID.String, p.Branch,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("promote edge: %v", err)}
			}
		}

		// Update the finding to closed.
		closedAt := time.Now().Unix()
		res, err := tx.ExecContext(ctx,
			`UPDATE findings
			    SET state = 'closed',
			        closed_reason = ?,
			        closed_at = ?,
			        actor_id = ?,
			        actor_kind = ?
			  WHERE finding_id = ? AND branch = ?`,
			p.Reason, closedAt, actor.ID, string(actor.Kind),
			p.FindingID, p.Branch,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("close finding: %v", err)}
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}

		if err := tx.Commit(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("commit tx: %v", err)}
		}

		if aw != nil {
			op := "finding.close"
			if isAccept {
				op = "finding.accept"
			}
			_ = aw.Write(ctx, ports.AuditEntry{
				RepoID:    p.RepoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        op,
				TargetID:  p.FindingID,
				Branch:    p.Branch,
				CreatedAt: time.Now(),
			})
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "closed",
		}, nil
	}
}
