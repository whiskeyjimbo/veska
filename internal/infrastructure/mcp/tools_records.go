package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
		InputSchema:     getFindingInputSchema,
		Handler:         makeGetFindingHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_suppression",
		Description:     "Get a single suppression by suppression_id.",
		IncludesStaging: false,
		InputSchema:     getSuppressionInputSchema,
		Handler:         makeGetSuppressionHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_close_suppression",
		Description:     "Terminate an active suppression now by setting expires_at to the current time.",
		IncludesStaging: false,
		InputSchema:     closeSuppressionInputSchema,
		Handler:         makeCloseSuppressionHandler(db, aw),
	})
}

// eng_get_finding

type getFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	// RepoID is accepted for symmetry with eng_list_findings
	// but the lookup is by finding_id alone — when supplied it is checked
	// against the row's repo_id and a mismatch returns NotFound.
	RepoID string `json:"repo_id"`
}

func makeGetFindingHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID); rpcErr != nil {
			return nil, rpcErr
		}

		// accept an unambiguous finding_id prefix (12+ chars in
		// the CLI listing).: finding_id is globally unique, so
		// branch is just a consistency hint; the resolver checks it when set.
		fullID, rpcErr := resolveFindingPrefix(ctx, db, p.FindingID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}
		var f findingRow
		err := db.QueryRowContext(ctx,
			`SELECT finding_id, branch, repo_id, node_id, file_path, severity, source_layer,
			        rule, message, state, closed_reason, created_at, closed_at, actor_id, actor_kind
			   FROM findings WHERE finding_id = ?`, fullID,
		).Scan(
			&f.FindingID, &f.Branch, &f.RepoID, &f.NodeID, &f.FilePath,
			&f.Severity, &f.SourceLayer, &f.Rule, &f.Message, &f.State,
			&f.ClosedReason, &f.CreatedAt, &f.ClosedAt, &f.ActorID, &f.ActorKind,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
		}
		// when --repo is supplied, ensure it agrees with the
		// row we loaded; mismatch is a NotFound (the agent asked for "this
		// finding scoped to repo X" and that pair does not exist).
		if p.RepoID != "" && !findingRepoMatches(f.RepoID, p.RepoID) {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s in repo %s", p.FindingID, p.RepoID)}
		}
		f.FilePath = relativizeFindingPath(f.FilePath, findingRepoRoot(ctx, db, f.RepoID))

		return map[string]any{"finding": f}, nil
	}
}

// findingPrefixQuerier is the subset of *sql.DB / *sql.Tx that the prefix
// resolver needs — lets close/reopen handlers resolve on their own tx so
// the row is locked through the subsequent UPDATE.
type findingPrefixQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// resolveFindingPrefix maps a finding_id prefix to its full id. Used so the
// MCP handlers (eng_get_finding, eng_close_finding, eng_reopen_finding,
// eng_suppress_finding) accept the 12-char short form `veska findings list`
// prints, mirroring the short-id resolution other tools already do
// Branch, when supplied, scopes the match.
// Returns the full finding_id when exactly one row matches; CodeNotFound
// for zero matches and CodeInvalidParams for an ambiguous prefix.
func resolveFindingPrefix(ctx context.Context, q findingPrefixQuerier, prefix, branch string) (string, *RPCError) {
	if prefix == "" {
		return "", &RPCError{Code: CodeInvalidParams, Message: "finding_id is required"}
	}
	query := `SELECT finding_id FROM findings WHERE finding_id LIKE ? || '%'`
	args := []any{prefix}
	if branch != "" {
		query += ` AND branch = ?`
		args = append(args, branch)
	}
	query += ` LIMIT 2`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query finding: %v", err)}
	}
	defer rows.Close()
	var matched []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan finding: %v", err)}
		}
		matched = append(matched, id)
	}
	if err := rows.Err(); err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("iterate findings: %v", err)}
	}
	if len(matched) == 0 {
		if branch != "" {
			return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s on branch %s", prefix, branch)}
		}
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("finding not found: %s", prefix)}
	}
	if len(matched) > 1 {
		return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("ambiguous finding_id prefix %q: matches multiple findings — supply more characters", prefix)}
	}
	return matched[0], nil
}

// findingRepoMatches returns true when supplied matches actual exactly or
// is a prefix of actual (mirrors the short-id matching eng_list_findings
// and eng_promote_repo already accept).
func findingRepoMatches(actual, supplied string) bool {
	if supplied == "" || actual == supplied {
		return true
	}
	return strings.HasPrefix(actual, supplied)
}

// eng_get_suppression

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

// eng_close_suppression

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
