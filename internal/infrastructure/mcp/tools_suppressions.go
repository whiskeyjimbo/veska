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

// RegisterSuppressionTools registers suppression management tools on r.
// db is the SQLite connection that backs the suppressions table.
// aw is an optional AuditWriter; pass nil to disable audit logging.
func RegisterSuppressionTools(r *Registry, db *sql.DB, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_suppress_finding",
		Description:     "Suppress a finding, inserting a record into the suppressions table.",
		IncludesStaging: false,
		Handler:         makeSuppressFindingHandler(db, aw),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_suppressions",
		Description:     "List suppressions for a given repo and branch.",
		IncludesStaging: false,
		Handler:         makeListSuppressionsHandler(db),
	})
}

// ---------------------------------------------------------------------------
// eng_suppress_finding
// ---------------------------------------------------------------------------

type suppressFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

func makeSuppressFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p suppressFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "branch", p.Branch, "repo_id", p.RepoID, "reason", p.Reason); rpcErr != nil {
			return nil, rpcErr
		}
		if p.Scope == "" {
			p.Scope = "finding"
		}

		supID := fmt.Sprintf("sup_%d", time.Now().UnixNano())
		createdAt := time.Now().Unix()

		_, err := db.ExecContext(ctx,
			`INSERT INTO suppressions
				(suppression_id, scope, target, branch, rule, reason, expires_at, created_at, actor_id, actor_kind)
			VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
			supID, p.Scope, p.FindingID, p.Branch, p.Reason, p.ExpiresAt, createdAt,
			actor.ID, string(actor.Kind),
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("insert suppression: %v", err)}
		}

		if aw != nil {
			_ = aw.Write(ctx, ports.AuditEntry{
				RepoID:    p.RepoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        "finding.suppress",
				TargetID:  p.FindingID,
				Branch:    p.Branch,
				CreatedAt: time.Now(),
			})
		}

		return map[string]any{
			"suppression_id": supID,
			"finding_id":     p.FindingID,
			"branch":         p.Branch,
			"scope":          p.Scope,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_list_suppressions
// ---------------------------------------------------------------------------

type listSuppressionsParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

type suppressionRow struct {
	SuppressionID string  `json:"suppression_id"`
	Scope         string  `json:"scope"`
	Target        string  `json:"target"`
	Branch        *string `json:"branch,omitempty"`
	Rule          *string `json:"rule,omitempty"`
	Reason        string  `json:"reason"`
	ExpiresAt     *int64  `json:"expires_at,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	ActorID       string  `json:"actor_id"`
	ActorKind     string  `json:"actor_kind"`
}

func makeListSuppressionsHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listSuppressionsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}

		// Suppressions are scoped by branch. We also include branch-NULL suppressions (repo-wide).
		rows, err := db.QueryContext(ctx,
			`SELECT suppression_id, scope, target, branch, rule, reason, expires_at, created_at, actor_id, actor_kind
			   FROM suppressions
			  WHERE branch = ? OR branch IS NULL
			  ORDER BY created_at DESC`,
			p.Branch,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query suppressions: %v", err)}
		}
		defer rows.Close()

		suppressions := make([]suppressionRow, 0)
		for rows.Next() {
			var s suppressionRow
			if err := rows.Scan(
				&s.SuppressionID, &s.Scope, &s.Target, &s.Branch, &s.Rule,
				&s.Reason, &s.ExpiresAt, &s.CreatedAt, &s.ActorID, &s.ActorKind,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan suppression: %v", err)}
			}
			suppressions = append(suppressions, s)
		}
		if err := rows.Err(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("iterate suppressions: %v", err)}
		}

		return map[string]any{
			"suppressions": suppressions,
		}, nil
	}
}
