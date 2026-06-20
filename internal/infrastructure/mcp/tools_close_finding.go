// SPDX-License-Identifier: AGPL-3.0-only

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

// CodeHumanRequired is returned when a severity >= high finding is closed by a non-human actor.
const CodeHumanRequired = -32001

// reviewPipelineFailureRule matches the rule of the sticky finding parked by
// the review pipeline when a review job exhausts its retries, allowing a user
// to clear the review queue. This constant is duplicated from the review
// package to avoid importing application logic here.
const reviewPipelineFailureRule = "review-pipeline-failure"

type closeFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
}

var closeFindingInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "finding_id": {"type": "string", "description": "ID of the finding to close."},
    "branch": {"type": "string", "description": "Branch the finding belongs to (optional; derived from finding_id when omitted)."},
    "repo_id": {"type": "string", "description": "Repository ID for audit attribution (optional; derived from finding_id when omitted)."},
    "reason": {"type": "string", "description": "Close reason; \"accept\" promotes an auto-link edge to definite."}
  },
  "required": ["finding_id", "reason"]
}`)

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

		// We execute the finding closure and edge promotion in a single transaction to guarantee atomic updates.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("begin tx: %v", err)}
		}
		defer func() { _ = tx.Rollback() }()

		// We resolve the finding ID prefix within the transaction to lock the row and prevent concurrent modifications.
		fullID, rpcErr := resolveFindingPrefix(ctx, tx, p.FindingID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.FindingID = fullID

		// We fetch the finding's details to validate its state, check severity restrictions, and resolve the branch or repository if they were not explicitly provided.
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
		if p.Branch != "" && p.Branch != rowBranch {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}
		p.Branch = rowBranch
		if p.RepoID == "" {
			p.RepoID = rowRepoID
		}

		// Severity levels of high or higher require a human actor to prevent automated scripts from closing critical findings without review.
		sev := domain.Severity(severity)
		if sev.AtLeast(domain.SeverityHigh) && actor.Kind != domain.ActorKindHuman {
			// We include a resolution hint in the error message to guide automated systems on how a human can perform this action.
			return nil, &RPCError{
				Code:    CodeHumanRequired,
				Message: fmt.Sprintf("human_required: severity=%s requires a human actor - close from the CLI as a human user (veska findings close %s --reason=...) or have a teammate run eng_close_finding", severity, p.FindingID),
				Data: map[string]any{
					"reason":     "human_required",
					"finding_id": p.FindingID,
					"severity":   severity,
					"hint":       "close from the human CLI (veska findings close) or have a human actor run eng_close_finding",
				},
			}
		}

		// When an auto-link finding is accepted, we promote the associated edge to 'definite' state. If the edge reference is missing or already definite, the operation soft-fails or succeeds silently to avoid blocking the closure of the finding itself.
		isAccept := rule == "auto-link" && p.Reason == "accept"
		if isAccept && nodeID.Valid && nodeID.String != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE edges SET confidence = 'definite' WHERE edge_id = ? AND branch = ?`,
				nodeID.String, p.Branch,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("promote edge: %v", err)}
			}
		}

		// Closing a review-pipeline-failure finding clears associated failed review jobs in the post-promotion queue by setting their state to 'done'. Missing anchors or missing queue rows are treated as soft no-ops so that the finding can always be closed.
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

		// We preserve the original actor fields on the finding record because they represent the creator, whereas the closer's identity is tracked separately in the audit log.
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
				Reason:    p.Reason,
			})
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "closed",
		}, nil
	}
}
