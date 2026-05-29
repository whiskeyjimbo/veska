package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterOwnerTools registers the eng_find_owner tool on r.
// db, when non-nil, is used to resolve a repo_id to its working-tree root
// (the repos table has root_path). When db is nil — the test path — repo_id
// is treated as a literal filesystem root.
// repos, when non-nil, is used by the handler to resolve repo_id from cwd
// or short_id and to give the standard 'N repos registered' hint on a
// missing-repo_id error (solov2-eq5a). When nil the handler falls back
// to the bare "repo_id is required" behaviour.
func RegisterOwnerTools(r *Registry, db *sql.DB, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_owner",
		Description:     "Find the owner of a file via CODEOWNERS lookup or git blame fallback.",
		IncludesStaging: false,
		InputSchema:     findOwnerInputSchema,
		Handler:         makeFindOwnerHandler(db, repos),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
}

// resolveOwnerRoot turns a repo_id into the on-disk working-tree path used
// for CODEOWNERS and git blame. The repos table has the canonical mapping;
// when the db lookup fails (no db, or repo_id is an unknown id), the input
// is returned as-is so direct callers that pass a path still work. Short
// repo_id (12 char) prefixes are accepted for parity with other tools
// (solov2-mha4).
func resolveOwnerRoot(db *sql.DB, repoID string) string {
	if db == nil {
		return repoID
	}
	var root string
	err := db.QueryRow(`SELECT root_path FROM repos WHERE repo_id = ?`, repoID).Scan(&root)
	if err == nil && root != "" {
		return root
	}
	// Try short_id prefix match (mirrors resolveRepoIDDB).
	rows, qerr := db.Query(`SELECT repo_id, root_path FROM repos`)
	if qerr == nil {
		defer rows.Close()
		for rows.Next() {
			var id, rp string
			if rows.Scan(&id, &rp) == nil && ShortRepoID(id) == repoID && rp != "" {
				return rp
			}
		}
	}
	return repoID
}

// ---------------------------------------------------------------------------
// eng_find_owner
// ---------------------------------------------------------------------------

type findOwnerParams struct {
	FilePath string `json:"file_path"`
	// Path is an accepted alias for FilePath, matching the precedent set by
	// eng_get_file_nodes. Users naturally reach for "path"; honouring both
	// keeps the MCP surface internally consistent (solov2-jtl5.10).
	Path string `json:"path"`
	// Symbol / NodeID resolve to the defining file's path before the
	// CODEOWNERS / blame lookup. Mirrors the symbol-or-node pattern other
	// eng_* tools accept (find_symbol, get_blast_radius, ...) — solov2-mmox.
	Symbol string `json:"symbol"`
	NodeID string `json:"node_id"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

func makeFindOwnerHandler(db *sql.DB, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findOwnerParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.FilePath == "" {
			p.FilePath = p.Path
		}
		// solov2-eq5a: when a RepoLister is wired, route through the
		// shared resolver so the missing-repo_id error carries the same
		// "N repos registered; pass eng_list_repos to find the id" hint
		// the peer tools give, and so single-repo callers get
		// auto-resolution + cwd-pin fallback for free. Test/no-DB
		// callers (repos == nil) keep the bare contract.
		if repos != nil {
			if id, rpcErr := resolveRepoIDOrCwd(ctx, repos, p.RepoID, cwdFromParams(raw)); rpcErr != nil {
				return nil, rpcErr
			} else {
				p.RepoID = id
			}
		} else if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}
		if p.FilePath == "" && (p.Symbol != "" || p.NodeID != "") {
			fp, ferr := lookupNodeFilePath(db, p.RepoID, p.Branch, p.Symbol, p.NodeID)
			if ferr != nil {
				return nil, ferr
			}
			p.FilePath = fp
		}
		if p.FilePath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path (or alias 'path'), or one of symbol/node_id, is required"}
		}

		root := resolveOwnerRoot(db, p.RepoID)

		// Step 1: try CODEOWNERS.
		if owner, ok := lookupCodeowners(root, p.FilePath); ok {
			return map[string]any{
				"owner":  owner,
				"source": "codeowners",
			}, nil
		}

		// Step 2: git blame fallback.
		if email, ok := gitBlameEmail(root, p.FilePath); ok {
			return map[string]any{
				"owner":  email,
				"source": "git_blame",
			}, nil
		}

		// Step 3: both failed. Surface a 'reason' so the caller can tell
		// 'no CODEOWNERS file' from 'file exists but covers nothing' from
		// 'git blame failed' (solov2-xjg). Cheap stat: just check whether
		// a CODEOWNERS file is present at either of the canonical paths.
		reason := codeownersAbsenceReason(root)
		return map[string]any{
			"owner":  nil,
			"source": nil,
			"reason": reason,
		}, nil
	}
}

// lookupNodeFilePath resolves a symbol or node_id to its defining file
// path under (repoID, branch), so eng_find_owner accepts the same
// symbol-or-node-id pattern as the rest of the eng_* surface
// (solov2-mmox). Returns (filePath, nil) on a single match; an empty
// string + RPCError when no row or ambiguous matches are found. branch
// may be empty: when so, we pick the row with the largest node_id and
// don't constrain the branch.
func lookupNodeFilePath(db *sql.DB, repoID, branch, symbol, nodeID string) (string, *RPCError) {
	if db == nil {
		return "", &RPCError{Code: CodeInternalError, Message: "find_owner: no database wired for symbol/node_id resolution"}
	}
	// Accept short_id (12-char prefix) for parity with the rest of the
	// eng_* surface — solov2-mha4 / solov2-mmox.
	if len(repoID) < 64 {
		rows, qerr := db.Query(`SELECT repo_id FROM repos`)
		if qerr == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil && (id == repoID || ShortRepoID(id) == repoID) {
					repoID = id
					break
				}
			}
			rows.Close()
		}
	}
	if nodeID != "" {
		// Accept full node_id or a short prefix (>=8 chars), mirroring
		// how other eng_* tools resolve node ids.
		var fp string
		err := db.QueryRow(`SELECT file_path FROM nodes WHERE repo_id = ? AND (node_id = ? OR node_id LIKE ?) LIMIT 1`, repoID, nodeID, nodeID+"%").Scan(&fp)
		if err == sql.ErrNoRows {
			return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("find_owner: node_id %s not found in repo %s", nodeID, repoID)}
		}
		if err != nil {
			return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_owner: node lookup: %v", err)}
		}
		return fp, nil
	}
	// Match the bare symbol against either the full symbol_path
	// ("pkg.Sym") or its trailing component ("Sym").
	args := []any{repoID, symbol, "%." + symbol}
	q := `SELECT DISTINCT file_path FROM nodes WHERE repo_id = ? AND (symbol_path = ? OR symbol_path LIKE ?)`
	if branch != "" {
		q += ` AND branch = ?`
		args = append(args, branch)
	}
	q += ` LIMIT 2`
	rows, err := db.Query(q, args...)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_owner: symbol lookup: %v", err)}
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err == nil {
			paths = append(paths, fp)
		}
	}
	switch len(paths) {
	case 0:
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("find_owner: symbol %q not found in repo %s", symbol, repoID)}
	case 1:
		return paths[0], nil
	default:
		return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("find_owner: symbol %q is ambiguous (multiple files match) — pass file_path or node_id", symbol)}
	}
}

// codeownersAbsenceReason explains why find_owner produced no owner.
// Used only on the null path; the message is for human / agent
// consumption, not parsed as a structured enum.
func codeownersAbsenceReason(repoRoot string) string {
	for _, path := range []string{
		filepath.Join(repoRoot, "CODEOWNERS"),
		filepath.Join(repoRoot, ".github", "CODEOWNERS"),
	} {
		if _, err := os.Stat(path); err == nil {
			return "CODEOWNERS exists but no rule matched this file; git blame also yielded no author"
		}
	}
	return "no CODEOWNERS file in repo root or .github/; git blame also yielded no author"
}

// lookupCodeowners searches for a CODEOWNERS file at repoRoot or repoRoot/.github,
// parses it, and returns the owner of the longest-matching pattern.
func lookupCodeowners(repoRoot, filePath string) (string, bool) {
	candidates := []string{
		filepath.Join(repoRoot, "CODEOWNERS"),
		filepath.Join(repoRoot, ".github", "CODEOWNERS"),
	}

	var lines []string
	for _, path := range candidates {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		f.Close()
		break
	}
	if len(lines) == 0 {
		return "", false
	}

	// Parse patterns. Last match wins (CODEOWNERS semantics: last matching rule wins).
	// We implement longest-pattern wins as a proxy since we want the most specific match.
	// The spec says "longest-matching glob wins", so we track pattern length.
	bestOwner := ""
	bestPatternLen := -1

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		pattern := parts[0]
		owner := parts[1]

		if matchesCodeownersPattern(pattern, filePath) {
			if len(pattern) > bestPatternLen {
				bestPatternLen = len(pattern)
				bestOwner = owner
			}
		}
	}

	if bestOwner == "" {
		return "", false
	}
	return bestOwner, true
}

// matchesCodeownersPattern checks whether filePath matches a CODEOWNERS pattern.
// Supports:
//   - "*"  wildcard (matches anything in any directory component, a la filepath.Match)
//   - Leading "/" anchors to repo root
//   - Trailing "/" matches a directory prefix
func matchesCodeownersPattern(pattern, filePath string) bool {
	// Normalise file path (remove leading /).
	fp := strings.TrimPrefix(filePath, "/")

	// Strip leading / from pattern to make it relative.
	anchored := strings.HasPrefix(pattern, "/")
	pat := strings.TrimPrefix(pattern, "/")

	// If pattern ends with /, treat as directory prefix match.
	if before, ok := strings.CutSuffix(pat, "/"); ok {
		dir := before
		if anchored {
			return strings.HasPrefix(fp, dir+"/") || fp == dir
		}
		return strings.Contains(fp, dir+"/") || strings.HasPrefix(fp, dir+"/")
	}

	if anchored {
		// Anchored: match against the full file path.
		matched, err := filepath.Match(pat, fp)
		if err == nil && matched {
			return true
		}
		// Also allow matching the filename within sub-paths.
		return false
	}

	// Unanchored: try matching the full path or just the filename component.
	matched, err := filepath.Match(pat, fp)
	if err == nil && matched {
		return true
	}
	// Try matching each path component.
	matched, err = filepath.Match(pat, filepath.Base(fp))
	if err == nil && matched {
		return true
	}
	// Try matching as a suffix (any directory).
	for _, seg := range pathSegments(fp) {
		if m, e := filepath.Match(pat, seg); e == nil && m {
			return true
		}
	}
	return false
}

// pathSegments returns progressively shorter suffix paths of fp.
// e.g. "a/b/c.go" → ["a/b/c.go", "b/c.go", "c.go"]
func pathSegments(fp string) []string {
	var segs []string
	for {
		segs = append(segs, fp)
		idx := strings.Index(fp, "/")
		if idx < 0 {
			break
		}
		fp = fp[idx+1:]
	}
	return segs
}

// gitBlameEmail runs git log to get the last committer's email for a file.
func gitBlameEmail(repoRoot, filePath string) (string, bool) {
	cmd := exec.Command("git", "-C", repoRoot, "log", "--follow", "-1", "--format=%ae", "--", filePath)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	email := strings.TrimSpace(string(out))
	if email == "" {
		return "", false
	}
	return email, true
}
