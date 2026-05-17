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

// RegisterRecordTools registers the single-record finding/suppression read
// tools plus eng_close_suppression on r. db backs the findings and
// suppressions tables; aw is an optional AuditWriter (pass nil to disable).
func RegisterRecordTools(r *Registry, db *sql.DB, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_get_finding",
		Description:     "Get a single finding by finding_id and branch.",
		IncludesStaging: false,
		Handler:         makeGetFindingHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_suppression",
		Description:     "Get a single suppression by suppression_id.",
		IncludesStaging: false,
		Handler:         makeGetSuppressionHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_close_suppression",
		Description:     "Terminate an active suppression now by setting expires_at to the current time.",
		IncludesStaging: false,
		Handler:         makeCloseSuppressionHandler(db, aw),
	})
}

// ---------------------------------------------------------------------------
// eng_get_finding
// ---------------------------------------------------------------------------

type getFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
}

func makeGetFindingHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}

		var f findingRow
		err := db.QueryRowContext(ctx,
			`SELECT finding_id, branch, repo_id, node_id, file_path, severity, source_layer,
				rule, message, state, closed_reason, created_at, closed_at, actor_id, actor_kind
			   FROM findings WHERE finding_id = ? AND branch = ?`,
			p.FindingID, p.Branch,
		).Scan(
			&f.FindingID, &f.Branch, &f.RepoID, &f.NodeID, &f.FilePath,
			&f.Severity, &f.SourceLayer, &f.Rule, &f.Message, &f.State,
			&f.ClosedReason, &f.CreatedAt, &f.ClosedAt, &f.ActorID, &f.ActorKind,
		)
		if err == sql.ErrNoRows {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
		}

		return map[string]any{"finding": f}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_get_suppression
// ---------------------------------------------------------------------------

type getSuppressionParams struct {
	SuppressionID string `json:"suppression_id"`
}

func makeGetSuppressionHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getSuppressionParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("suppression_id", p.SuppressionID); rpcErr != nil {
			return nil, rpcErr
		}

		var s suppressionRow
		err := db.QueryRowContext(ctx,
			`SELECT suppression_id, scope, target, branch, rule, reason, expires_at, created_at, actor_id, actor_kind
			   FROM suppressions WHERE suppression_id = ?`,
			p.SuppressionID,
		).Scan(
			&s.SuppressionID, &s.Scope, &s.Target, &s.Branch, &s.Rule,
			&s.Reason, &s.ExpiresAt, &s.CreatedAt, &s.ActorID, &s.ActorKind,
		)
		if err == sql.ErrNoRows {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("suppression not found: %s", p.SuppressionID)}
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query suppression: %v", err)}
		}

		return map[string]any{"suppression": s}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_close_suppression
// ---------------------------------------------------------------------------

type closeSuppressionParams struct {
	SuppressionID string `json:"suppression_id"`
	RepoID        string `json:"repo_id,omitempty"`
}

func makeCloseSuppressionHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p closeSuppressionParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("suppression_id", p.SuppressionID); rpcErr != nil {
			return nil, rpcErr
		}

		now := time.Now().Unix()
		res, err := db.ExecContext(ctx,
			`UPDATE suppressions SET expires_at = ? WHERE suppression_id = ?`,
			now, p.SuppressionID,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("close suppression: %v", err)}
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("suppression not found: %s", p.SuppressionID)}
		}

		if aw != nil {
			_ = aw.Write(ctx, ports.AuditEntry{
				RepoID:    p.RepoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        "suppression.close",
				TargetID:  p.SuppressionID,
				CreatedAt: time.Now(),
			})
		}

		return map[string]any{
			"suppression_id": p.SuppressionID,
			"expires_at":     now,
		}, nil
	}
}
