package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
	// Fall back to a unique short_id prefix match.
	rows, qerr := db.QueryContext(ctx, `SELECT repo_id FROM repos`)
	if qerr != nil {
		return repoID, nil
	}
	defer rows.Close()
	var matched string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("resolve repo_id: %v", serr)}
		}
		if ShortRepoID(id) == repoID {
			if matched != "" {
				return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("ambiguous short repo_id %q matches multiple repos", repoID)}
			}
			matched = id
		}
	}
	if err := rows.Err(); err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("resolve repo_id: %v", err)}
	}
	if matched != "" {
		return matched, nil
	}
	return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("unknown repo_id: %s (run eng_list_repos)", repoID)}
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
		repoID, rpcErr := resolveRepoIDDB(ctx, db, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
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
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan finding: %v", err)}
			}
			f.FilePath = relativizeFindingPath(f.FilePath, root)
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
