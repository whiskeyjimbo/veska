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

type reopenFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	// Reason is an optional justification recorded in the audit log.
	Reason string `json:"reason"`
}

func makeReopenFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p reopenFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID); rpcErr != nil {
			return nil, rpcErr
		}


		fullID, rpcErr := resolveFindingPrefix(ctx, db, p.FindingID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.FindingID = fullID


		var rowBranch, rowRepoID string
		err := db.QueryRowContext(ctx,
			`SELECT branch, repo_id FROM findings WHERE finding_id = ?`,
			p.FindingID,
		).Scan(&rowBranch, &rowRepoID)
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

		res, err := db.ExecContext(ctx,
			`UPDATE findings SET state = 'open', closed_at = NULL, closed_reason = NULL
			  WHERE finding_id = ? AND branch = ?`,
			p.FindingID, p.Branch,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("reopen finding: %v", err)}
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}

		if aw != nil {
			_ = aw.Write(ctx, ports.AuditEntry{
				RepoID:    p.RepoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        "finding.reopen",
				TargetID:  p.FindingID,
				Branch:    p.Branch,
				CreatedAt: time.Now(),
				Reason:    p.Reason,
			})
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "open",
		}, nil
	}
}
