package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// CodeHumanRequired is the custom JSON-RPC error code returned when a
// severity >= high finding is closed by a non-human actor.
const CodeHumanRequired = -32001

// RegisterFindingTools registers finding management tools on r.
// db is the SQLite connection that backs the findings table.
func RegisterFindingTools(r *Registry, db *sql.DB) {
	r.MustRegister(ToolSpec{
		Name:            "eng_close_finding",
		Description:     "Close a finding by ID. Severity >= high requires a human actor.",
		IncludesStaging: false,
		Handler:         makeCloseFindingHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_findings",
		Description:     "List findings for a repo and branch, optionally filtered by state or severity.",
		IncludesStaging: false,
		Handler:         makeListFindingsHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_reopen_finding",
		Description:     "Reopen a previously closed finding by ID.",
		IncludesStaging: false,
		Handler:         makeReopenFindingHandler(db),
	})
}

// ---------------------------------------------------------------------------
// eng_close_finding
// ---------------------------------------------------------------------------

type closeFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
}

func makeCloseFindingHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p closeFindingParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.FindingID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "finding_id is required"}
		}
		if p.Branch == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "branch is required"}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}
		if p.Reason == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "reason is required"}
		}

		// Fetch the finding to check existence and severity.
		var severity string
		var state string
		err := db.QueryRowContext(ctx,
			`SELECT severity, state FROM findings WHERE finding_id = ? AND branch = ?`,
			p.FindingID, p.Branch,
		).Scan(&severity, &state)
		if err == sql.ErrNoRows {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
		}

		// Human-action gate: severity >= high requires a human actor.
		sev := domain.Severity(severity)
		if sev.AtLeast(domain.SeverityHigh) && actor.Kind != domain.ActorKindHuman {
			return nil, &RPCError{
				Code: CodeHumanRequired,
				Message: fmt.Sprintf(
					`{"reason":"human_required","finding_id":%q,"severity":%q}`,
					p.FindingID, severity,
				),
			}
		}

		// Update the finding to closed.
		closedAt := time.Now().Unix()
		res, err := db.ExecContext(ctx,
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
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "closed",
		}, nil
	}
}

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
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}
		if p.Branch == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "branch is required"}
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

// ---------------------------------------------------------------------------
// eng_reopen_finding
// ---------------------------------------------------------------------------

type reopenFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
}

func makeReopenFindingHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p reopenFindingParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.FindingID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "finding_id is required"}
		}
		if p.Branch == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "branch is required"}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
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
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("finding not found: %s on branch %s", p.FindingID, p.Branch)}
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "open",
		}, nil
	}
}
