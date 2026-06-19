// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RegisterSuppressionTools registers tools for managing finding suppressions.
func RegisterSuppressionTools(r *Registry, db *sql.DB, aw ports.AuditWriter, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_suppress_finding",
		Description:     "Suppress a finding, inserting a record into the suppressions table.",
		IncludesStaging: false,
		Handler:         makeSuppressFindingHandler(db, aw),
		InputSchema:     suppressFindingInputSchema,
		OutputSchema:    suppressFindingOutputSchema,
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_suppressions",
		Description:     "List suppressions for a given repo and branch. repo_id is optional when exactly one repo is registered (it auto-resolves); otherwise it is required.",
		IncludesStaging: false,
		InputSchema:     listSuppressionsInputSchema,
		Handler:         makeListSuppressionsHandler(db, repos),
	})
}

type suppressFindingParams struct {
	FindingID string `json:"finding_id"`
	Branch    string `json:"branch"`
	RepoID    string `json:"repo_id"`
	Reason    string `json:"reason"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

var suppressFindingInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "finding_id": {"type": "string", "description": "ID of the finding to suppress."},
    "branch": {"type": "string", "description": "Branch the finding belongs to (optional; derived from finding_id when scope='finding')."},
    "repo_id": {"type": "string", "description": "Repository ID for audit attribution (optional; derived from finding_id when scope='finding')."},
    "reason": {"type": "string", "description": "Reason for the suppression."},
    "scope": {"type": "string", "description": "Suppression scope; defaults to \"finding\" when omitted."},
    "expires_at": {"type": ["integer", "null"], "description": "Optional Unix timestamp at which the suppression expires."}
  },
  "required": ["finding_id", "reason"]
}`)

var suppressFindingOutputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "suppression_id": {"type": "string"},
    "finding_id": {"type": "string"},
    "branch": {"type": "string"},
    "scope": {"type": "string"}
  },
  "required": ["suppression_id", "finding_id", "branch", "scope"]
}`)

func makeSuppressFindingHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p suppressFindingParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("finding_id", p.FindingID, "reason", p.Reason); rpcErr != nil {
			return nil, rpcErr
		}
		if p.Scope == "" {
			p.Scope = "finding"
		}

		if p.Scope == "finding" {

			fullID, rpcErr := resolveFindingPrefix(ctx, db, p.FindingID, p.Branch)
			if rpcErr != nil {

				if rpcErr.Code == CodeNotFound {
					rpcErr.Code = CodeInvalidParams
				}
				return nil, rpcErr
			}
			p.FindingID = fullID
			var rowBranch, rowRepoID string
			err := db.QueryRowContext(ctx,
				`SELECT branch, repo_id FROM findings WHERE finding_id = ? LIMIT 1`,
				p.FindingID,
			).Scan(&rowBranch, &rowRepoID)
			switch {
			case err == sql.ErrNoRows:
				return nil, &RPCError{
					Code:    CodeInvalidParams,
					Message: fmt.Sprintf("finding not found: %s", p.FindingID),
				}
			case err != nil:
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("lookup finding: %v", err)}
			}
			if p.Branch != "" && p.Branch != rowBranch {
				return nil, &RPCError{
					Code:    CodeInvalidParams,
					Message: fmt.Sprintf("finding %s is on branch %s, not %s", p.FindingID, rowBranch, p.Branch),
				}
			}
			if p.RepoID != "" && !findingRepoMatches(rowRepoID, p.RepoID) {
				return nil, &RPCError{
					Code:    CodeInvalidParams,
					Message: fmt.Sprintf("finding %s belongs to repo %s, not %s", p.FindingID, rowRepoID, p.RepoID),
				}
			}
			p.Branch = rowBranch
			p.RepoID = rowRepoID
		} else if rpcErr := checkRequired("branch", p.Branch, "repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
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

func makeListSuppressionsHandler(db *sql.DB, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listSuppressionsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}

		if p.RepoID != "" {
			if _, rpcErr := resolveRepoIDOrSingleton(ctx, repos, p.RepoID); rpcErr != nil {
				return nil, rpcErr
			}
		}

		query := `SELECT suppression_id, scope, target, branch, rule, reason, expires_at, created_at, actor_id, actor_kind
			   FROM suppressions`
		var args []any
		if p.Branch != "" {
			query += ` WHERE branch = ? OR branch IS NULL`
			args = append(args, p.Branch)
		}
		query += ` ORDER BY created_at DESC`
		rows, err := db.QueryContext(ctx, query, args...)
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
