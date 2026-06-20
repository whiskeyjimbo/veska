// SPDX-License-Identifier: AGPL-3.0-only

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

// RegisterOwnerTools registers the eng_find_owner tool.
func RegisterOwnerTools(r *Registry, db *sql.DB, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_owner",
		Description:     "Find the owner of a file via CODEOWNERS lookup or git blame fallback.",
		IncludesStaging: false,
		InputSchema:     findOwnerInputSchema,
		Handler:         makeFindOwnerHandler(db, repos),
	})
}

// resolveOwnerRoot resolves a repository ID to its absolute filesystem root path.
func resolveOwnerRoot(db *sql.DB, repoID string) string {
	if db == nil {
		return repoID
	}
	var root string
	err := db.QueryRow(`SELECT root_path FROM repos WHERE repo_id = ?`, repoID).Scan(&root)
	if err == nil && root != "" {
		return root
	}

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

type findOwnerParams struct {
	FilePath string `json:"file_path"`
	// Path is supported as an alias for FilePath.
	Path string `json:"path"`
	// Symbol/NodeID are parsed to resolve to the defining file path.
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

		if owner, ok := lookupCodeowners(root, p.FilePath); ok {
			return map[string]any{
				"owner":  owner,
				"source": "codeowners",
			}, nil
		}

		if email, ok := gitBlameEmail(root, p.FilePath); ok {
			return map[string]any{
				"owner":  email,
				"source": "git_blame",
			}, nil
		}

		reason := codeownersAbsenceReason(root)
		return map[string]any{
			"owner":  nil,
			"source": nil,
			"reason": reason,
		}, nil
	}
}

// lookupNodeFilePath resolves a symbol or node ID to its defining file path under the specified repository and branch.
func lookupNodeFilePath(db *sql.DB, repoID, branch, symbol, nodeID string) (string, *RPCError) {
	if db == nil {
		return "", &RPCError{Code: CodeInternalError, Message: "find_owner: no database wired for symbol/node_id resolution"}
	}

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
		return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("find_owner: symbol %q is ambiguous (multiple files match) - pass file_path or node_id", symbol)}
	}
}

// codeownersAbsenceReason details why find_owner could not locate an owner.
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

// lookupCodeowners parses CODEOWNERS configurations and matches the target path against the rules.
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

	// Longest matching pattern is selected to find the most specific rule.
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

// matchesCodeownersPattern returns whether the file path matches a CODEOWNERS glob pattern.
func matchesCodeownersPattern(pattern, filePath string) bool {
	// Normalize file path (remove leading /).
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

// pathSegments returns suffix paths of the file path.
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

// gitBlameEmail runs git log to retrieve the last committer's email.
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
