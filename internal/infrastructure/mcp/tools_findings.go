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

// RegisterFindingTools registers finding management tools on r.
// db is the SQLite connection that backs the findings table.
// aw is an optional AuditWriter; pass nil to disable audit logging.
func RegisterFindingTools(r *Registry, db *sql.DB, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_close_finding",
		Description:     "Close a finding by ID. Severity >= high requires a human actor.",
		IncludesStaging: false,
		Handler:         makeCloseFindingHandler(db, aw),
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
		Handler:         makeReopenFindingHandler(db, aw),
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

// ---------------------------------------------------------------------------
// eng_reopen_finding
// ---------------------------------------------------------------------------

type reopenFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
}

func makeReopenFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p reopenFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "branch", p.Branch, "repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
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
			})
		}

		return map[string]any{
			"finding_id": p.FindingID,
			"branch":     p.Branch,
			"state":      "open",
		}, nil
	}
}
