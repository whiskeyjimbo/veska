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

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterOwnerTools registers the eng_find_owner tool on r.
// db is accepted for consistency with other Register* functions but is not used.
func RegisterOwnerTools(r *Registry, _ *sql.DB) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_owner",
		Description:     "Find the owner of a file via CODEOWNERS lookup or git blame fallback.",
		IncludesStaging: false,
		Handler:         makeFindOwnerHandler(),
	})
}

// ---------------------------------------------------------------------------
// eng_find_owner
// ---------------------------------------------------------------------------

type findOwnerParams struct {
	FilePath string `json:"file_path"`
	RepoID   string `json:"repo_id"`
}

func makeFindOwnerHandler() ToolHandler {
	return func(_ context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findOwnerParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.FilePath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path is required"}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}

		// Step 1: try CODEOWNERS.
		if owner, ok := lookupCodeowners(p.RepoID, p.FilePath); ok {
			return map[string]any{
				"owner":  owner,
				"source": "codeowners",
			}, nil
		}

		// Step 2: git blame fallback.
		if email, ok := gitBlameEmail(p.RepoID, p.FilePath); ok {
			return map[string]any{
				"owner":  email,
				"source": "git_blame",
			}, nil
		}

		// Step 3: both failed. Surface a 'reason' so the caller can tell
		// 'no CODEOWNERS file' from 'file exists but covers nothing' from
		// 'git blame failed' (solov2-xjg). Cheap stat: just check whether
		// a CODEOWNERS file is present at either of the canonical paths.
		reason := codeownersAbsenceReason(p.RepoID)
		return map[string]any{
			"owner":  nil,
			"source": nil,
			"reason": reason,
		}, nil
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
