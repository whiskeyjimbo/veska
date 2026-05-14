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

// RegisterFindingTools registers the eng_close_finding tool on r.
// db is the SQLite connection that backs the findings table.
func RegisterFindingTools(r *Registry, db *sql.DB) {
	r.MustRegister(ToolSpec{
		Name:            "eng_close_finding",
		Description:     "Close a finding by ID. Severity >= high requires a human actor.",
		IncludesStaging: false,
		Handler:         makeCloseFindingHandler(db),
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
