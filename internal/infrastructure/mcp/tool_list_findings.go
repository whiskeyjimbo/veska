package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// relativizeFindingPath normalizes a finding's stored file_path to a
// repo-root-relative form. Findings are anchored at different layers — the
// checks pipeline stores repo-relative paths while the ingester (cold scan)
// stores absolute ones — so the wire contract is unified here at the read
// boundary instead (solov2-62gc). A nil path (e.g. auto-link findings, which
// anchor on an edge, not a file) is left untouched.
func relativizeFindingPath(path *string, root string) *string {
	if path == nil || root == "" || !filepath.IsAbs(*path) {
		return path
	}
	rel, err := filepath.Rel(root, *path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return &rel
}

// findingRepoRoot looks up the working-tree root for repoID from the repos
// table; "" when unknown so relativizeFindingPath is a no-op.
func findingRepoRoot(ctx context.Context, db *sql.DB, repoID string) string {
	var root string
	if err := db.QueryRowContext(ctx, `SELECT root_path FROM repos WHERE repo_id = ?`, repoID).Scan(&root); err != nil {
		return ""
	}
	return root
}

// resolveRepoIDDB canonicalizes repoID against the repos table the same way
// resolveRepoID does for RepoLister-backed tools (solov2-s7k0): an exact match
// wins; otherwise a unique short_id (ShortRepoIDLen-char) prefix is accepted.
// Findings-family tools query the DB directly and have no RepoLister, so this
// keeps the short_id contract uniform across the surface. Unknown/ambiguous
// ids surface as a loud RPCError rather than a silently-empty result.
func resolveRepoIDDB(ctx context.Context, db *sql.DB, repoID string) (string, *RPCError) {
	var exact string
	err := db.QueryRowContext(ctx, `SELECT repo_id FROM repos WHERE repo_id = ?`, repoID).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if err != sql.ErrNoRows {
		// repos table unavailable (e.g. a minimal test DB) — skip validation
		// and pass the id through unchanged, never worse than pre-resolution.
		return repoID, nil
	}
	// Pull the full set once so we can run both the short_id match and the
	// prefix match against the same snapshot (solov2-rkbc).
	rows, qerr := db.QueryContext(ctx, `SELECT repo_id FROM repos`)
	if qerr != nil {
		return repoID, nil
	}
	defer rows.Close()
	var allIDs []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("resolve repo_id: %v", serr)}
		}
		allIDs = append(allIDs, id)
	}
	if err := rows.Err(); err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("resolve repo_id: %v", err)}
	}
	// Exact short_id match.
	for _, id := range allIDs {
		if ShortRepoID(id) == repoID {
			return id, nil
		}
	}
	// Unambiguous prefix (>= minRepoIDPrefix chars).
	if len(repoID) >= minRepoIDPrefix {
		var matched string
		for _, id := range allIDs {
			if strings.HasPrefix(id, repoID) {
				if matched != "" {
					return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("ambiguous repo_id prefix %q matches multiple repos", repoID)}
				}
				matched = id
			}
		}
		if matched != "" {
			return matched, nil
		}
	}
	return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("unknown repo_id: %s (run eng_list_repos; prefixes must be >= %d chars)", repoID, minRepoIDPrefix)}
}

// ---------------------------------------------------------------------------
// eng_list_findings
// ---------------------------------------------------------------------------

type listFindingsParams struct {
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
	State    string `json:"state,omitempty"`
	Severity string `json:"severity,omitempty"`
	Rule     string `json:"rule,omitempty"`
	// IncludeSuppressed surfaces findings hidden by an active suppression
	// row. Default false matches the user expectation that
	// eng_suppress_finding actually suppresses (solov2-2ye2).
	IncludeSuppressed bool `json:"include_suppressed,omitempty"`
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
	// SuppressedBy carries the suppression_id when an active suppression
	// is hiding this finding. Populated only when IncludeSuppressed=true
	// (solov2-2ye2).
	SuppressedBy *string `json:"suppressed_by,omitempty"`
}

func makeListFindingsHandler(db *sql.DB, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listFindingsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ig2x: fall back to a cwd-injected hint when repo_id is
		// omitted, matching the other repo-scoped query tools. A nil
		// RepoLister preserves the old "repo_id is required" behaviour so
		// test sites that don't care about resolution can still pass nil.
		if p.RepoID == "" && repos != nil {
			resolved, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, "")
			if rpcErr != nil {
				return nil, rpcErr
			}
			p.RepoID = resolved
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}
		repoID, rpcErr := resolveRepoIDDB(ctx, db, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if p.State == "" {
			p.State = "open"
		}

		// LEFT JOIN against active suppressions so we can either filter out
		// suppressed findings (default) or surface them with a suppressed_by
		// hint (when include_suppressed=true). An "active" suppression is one
		// whose expires_at is NULL or in the future — eng_close_suppression
		// terminates by setting expires_at = now (solov2-2ye2).
		nowMS := time.Now().UnixMilli()
		query := `SELECT f.finding_id, f.branch, f.repo_id, f.node_id, f.file_path, f.severity, f.source_layer,
			f.rule, f.message, f.state, f.closed_reason, f.created_at, f.closed_at, f.actor_id, f.actor_kind,
			s.suppression_id
			FROM findings f
			LEFT JOIN suppressions s
			  ON s.scope = 'finding' AND s.target = f.finding_id
			 AND (s.expires_at IS NULL OR s.expires_at > ?)
			WHERE f.repo_id = ? AND f.state = ?`
		args := []any{nowMS, p.RepoID, p.State}
		if !p.IncludeSuppressed {
			query += ` AND s.suppression_id IS NULL`
		}
		if p.Branch != "" {
			query += ` AND f.branch = ?`
			args = append(args, p.Branch)
		}
		if p.Severity != "" {
			query += ` AND f.severity = ?`
			args = append(args, p.Severity)
		}
		if p.Rule != "" {
			query += ` AND f.rule = ?`
			args = append(args, p.Rule)
		}

		// Resolve the repo root BEFORE opening the findings cursor: on the
		// single-connection write pool, a second query while rows is open
		// deadlocks (the cursor holds the only connection).
		root := findingRepoRoot(ctx, db, p.RepoID)

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
				&f.SuppressedBy,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan finding: %v", err)}
			}
			f.FilePath = relativizeFindingPath(f.FilePath, root)
			findings = append(findings, f)
		}
		if err := rows.Err(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("iterate findings: %v", err)}
		}

		// degraded_reasons is always emitted (as [] when nothing is degraded)
		// to match the README's "Conventions across the tool surface" contract
		// (solov2-7cw7).
		return map[string]any{
			"findings":         findings,
			"degraded_reasons": []string{},
		}, nil
	}
}
