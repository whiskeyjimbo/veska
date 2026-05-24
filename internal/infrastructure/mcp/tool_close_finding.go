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

// reviewPipelineFailureRule is the rule string of the sticky finding parked by
// the review pipeline when a review job exhausts its retries. Closing such a
// finding flips its parked review-queue rows to 'done'. It mirrors
// review.FailureRule; the constant is duplicated to keep this infrastructure
// adapter free of an application-package import.
const reviewPipelineFailureRule = "review-pipeline-failure"

// ---------------------------------------------------------------------------
// eng_close_finding
// ---------------------------------------------------------------------------

type closeFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
}

// closeFindingInputSchema describes the params object for eng_close_finding.
var closeFindingInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "finding_id": {"type": "string", "description": "ID of the finding to close."},
    "branch": {"type": "string", "description": "Branch the finding belongs to (optional; derived from finding_id when omitted)."},
    "repo_id": {"type": "string", "description": "Repository ID for audit attribution (optional; derived from finding_id when omitted)."},
    "reason": {"type": "string", "description": "Close reason; \"accept\" promotes an auto-link edge to definite."}
  },
  "required": ["finding_id", "reason"]
}`)

// closeFindingOutputSchema describes the result object for eng_close_finding.
var closeFindingOutputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "finding_id": {"type": "string"},
    "branch": {"type": "string"},
    "state": {"type": "string", "const": "closed"}
  },
  "required": ["finding_id", "branch", "state"]
}`)

func makeCloseFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p closeFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "reason", p.Reason); rpcErr != nil {
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
		// finding_id is globally unique; branch/repo_id are looked up when not
		// supplied so callers can address findings by id alone.
		var (
			severity  string
			state     string
			rule      string
			nodeID    sql.NullString
			rowBranch string
			rowRepoID string
		)
		err = tx.QueryRowContext(ctx,
			`SELECT severity, state, rule, node_id, branch, repo_id FROM findings WHERE finding_id = ?`,
			p.FindingID,
		).Scan(&severity, &state, &rule, &nodeID, &rowBranch, &rowRepoID)
		if err == sql.ErrNoRows {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s", p.FindingID)}
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
		}
		// If caller passed branch/repo_id, honor them as a consistency check;
		// otherwise fall back to the row's own values.
		if p.Branch != "" && p.Branch != rowBranch {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}
		p.Branch = rowBranch
		if p.RepoID == "" {
			p.RepoID = rowRepoID
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

		// review-pipeline-failure close-flips-row (solov2-nz2.3 AC2): closing
		// the sticky review-failure finding clears its parked review jobs by
		// flipping every still-failed review row anchored on the promotion
		// commit (node_id carries the git_sha) to state='done', in the same tx.
		// The human-action gate above already enforces a human closer (the
		// finding is severity high). A missing/empty anchor or zero matched
		// rows is a soft no-op — the finding still closes.
		if rule == reviewPipelineFailureRule && nodeID.Valid && nodeID.String != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE post_promotion_queue
				    SET state = 'done'
				  WHERE work_kind = 'review'
				    AND repo_id = ?
				    AND branch = ?
				    AND git_sha = ?
				    AND state = 'failed'`,
				p.RepoID, p.Branch, nodeID.String,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("clear review queue rows: %v", err)}
			}
		}

		// Update the finding to closed.
		//
		// solov2-iyog: do NOT overwrite actor_id/actor_kind on close. The
		// finding's actor columns mean "who created/last-saved this finding"
		// — clobbering them with the closer caused TODO findings (created by
		// service:veska) to surface as actor_id=agent:unknown after an MCP
		// caller closed them, even though the creator never changed. The
		// audit log below already records who performed the close.
		closedAt := time.Now().Unix()
		res, err := tx.ExecContext(ctx,
			`UPDATE findings
			    SET state = 'closed',
			        closed_reason = ?,
			        closed_at = ?
			  WHERE finding_id = ? AND branch = ?`,
			p.Reason, closedAt,
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
