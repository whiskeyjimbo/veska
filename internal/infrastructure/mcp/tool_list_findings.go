package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// eng_list_findings
// ---------------------------------------------------------------------------

type listFindingsParams struct {
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
	State    string `json:"state,omitempty"`
	Severity string `json:"severity,omitempty"`
}

type findingRow struct {
	FindingID    string  `json:"finding_id"`
	Branch       string  `json:"branch"`
	RepoID       string  `json:"repo_id"`
	NodeID       *string `json:"node_id,omitempty"`
	FilePath     *string `json:"file_path,omitempty"`
	Severity     string  `json:"severity"`
	SourceLayer  string  `json:"source_layer"`
	Rule         string  `json:"rule"`
	Message      string  `json:"message"`
	State        string  `json:"state"`
	ClosedReason *string `json:"closed_reason,omitempty"`
	CreatedAt    int64   `json:"created_at"`
	ClosedAt     *int64  `json:"closed_at,omitempty"`
	ActorID      string  `json:"actor_id"`
	ActorKind    string  `json:"actor_kind"`
}

func makeListFindingsHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listFindingsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
		if p.State == "" {
			p.State = "open"
		}

		query := `SELECT finding_id, branch, repo_id, node_id, file_path, severity, source_layer,
			rule, message, state, closed_reason, created_at, closed_at, actor_id, actor_kind
			FROM findings WHERE repo_id = ? AND branch = ? AND state = ?`
		args := []any{p.RepoID, p.Branch, p.State}
		if p.Severity != "" {
			query += ` AND severity = ?`
			args = append(args, p.Severity)
		}

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query findings: %v", err)}
		}
		defer rows.Close()

		findings := make([]findingRow, 0)
		for rows.Next() {
			var f findingRow
			if err := rows.Scan(
				&f.FindingID, &f.Branch, &f.RepoID, &f.NodeID, &f.FilePath,
				&f.Severity, &f.SourceLayer, &f.Rule, &f.Message, &f.State,
				&f.ClosedReason, &f.CreatedAt, &f.ClosedAt, &f.ActorID, &f.ActorKind,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan finding: %v", err)}
			}
			findings = append(findings, f)
		}
		if err := rows.Err(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("iterate findings: %v", err)}
		}

		return map[string]any{
			"findings": findings,
		}, nil
	}
}
