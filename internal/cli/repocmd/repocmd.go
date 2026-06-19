// SPDX-License-Identifier: AGPL-3.0-only

// Package repocmd holds the business logic behind the `veska repo` command
// family: repo add/remove/list orchestration, daemon MCP dialing, scan-progress
// watching + daemon-log tailing, repo-id resolution, and table formatting.
// cmd/veska/repo.go is reduced to Cobra command construction whose RunE bodies
// are thin calls into this package (, following the
// cmd = glue / logic-in-packages pattern from ). The shared CLI
// helpers (ShortRepoID, OpenLocalDB, ResolveCLIRepoID, ResolveRepoArg,
// FetchScanProgress, PromptDeps) live here too because several other cmd/veska
// commands depend on them.
package repocmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// RepoView is the row shape used by both the daemon path (decoded from
// eng_list_repos) and the direct-DB fallback path. Field names match the
// MCP response so json.Unmarshal works as-is.
type RepoView struct {
	RepoID          string `json:"repo_id"`
	RootPath        string `json:"root_path"`
	ActiveBranch    string `json:"active_branch"`
	LastPromotedSHA string `json:"last_promoted_sha"`
	Kind            string `json:"kind"` // Aliases is the list of user-defined human-friendly names for this
	// repo. Surfaced in the ALIAS column of `veska repo
	// list` and accepted anywhere a repo_id is expected.
	Aliases []string `json:"aliases"`
}

// listResult decodes the eng_list_repos MCP response.
type listResult struct {
	Repos []RepoView `json:"repos"`
}

// ShortRepoID returns the first 12 chars of a repo id - the alias shown by
// `veska repo list` and accepted anywhere a repo_id is required. The CLI
// surfaces this form so users copy the same token the tools expect, instead
// of the unwieldy 64-char canonical id.
func ShortRepoID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cliMinRepoIDPrefix mirrors mcp.minRepoIDPrefix - see that constant for the
// reasoning.
const cliMinRepoIDPrefix = 4

// ResolveCLIRepoID matches the MCP resolveRepoID progression for CLI callers:
// exact full id, then 12-char short_id, then user-set alias,
// then unambiguous prefix (>= 4 chars). Aliases beat prefix so a typed
// alias never gets shadowed by a colliding hex prefix.
// Returns a typed error so CLI commands can wrap it with their own prefix
// ("wiki: ", "reindex: ", etc.).
func ResolveCLIRepoID(records []repo.Record, repoID string) (repo.Record, error) {
	for _, r := range records {
		if r.RepoID == repoID {
			return r, nil
		}
	}
	for _, r := range records {
		if ShortRepoID(r.RepoID) == repoID {
			return r, nil
		}
	}
	for _, r := range records {
		if slices.Contains(r.Aliases, repoID) {
			return r, nil
		}
	}
	if matched, found, err := resolveByPrefix(records, repoID); err != nil || found {
		return matched, err
	}
	if len(repoID) < cliMinRepoIDPrefix {
		return repo.Record{}, fmt.Errorf("repo %q is not registered (prefixes must be >= %d chars)", repoID, cliMinRepoIDPrefix)
	}
	return repo.Record{}, fmt.Errorf("repo %q is not registered (no match by full id, short_id, alias, or prefix)", repoID)
}

// resolveByPrefix matches repoID as an unambiguous (>=4-char) hex prefix.
// found=false means no prefix match was attempted or none matched.
func resolveByPrefix(records []repo.Record, repoID string) (repo.Record, bool, error) {
	if len(repoID) < cliMinRepoIDPrefix {
		return repo.Record{}, false, nil
	}
	var matched repo.Record
	found := false
	for _, r := range records {
		if strings.HasPrefix(r.RepoID, repoID) {
			if found {
				return repo.Record{}, false, fmt.Errorf("ambiguous repo_id prefix %q matches multiple repos", repoID)
			}
			matched, found = r, true
		}
	}
	return matched, found, nil
}

// ResolveRepoArg returns the canonical repo_id for arg. A hex-only string
// (repo_id or short_id prefix) is returned unchanged - the registry already
// resolves prefixes. Anything else is treated as a filesystem path: it is
// resolved to absolute form and matched against the RootPath of every
// registered repo. The not-found error mentions the resolved abs path so
// the user sees what we actually looked up.
func ResolveRepoArg(ctx context.Context, arg string) (string, error) {
	if looksLikeRepoID(arg) {
		return arg, nil
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", arg, err)
	}
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return "", err
	}
	defer closeFn()
	records, err := repo.List(ctx, db)
	if err != nil {
		return "", fmt.Errorf("list registered repos: %w", err)
	}
	for _, r := range records {
		if r.RootPath == abs {
			return r.RepoID, nil
		}
	}
	return "", fmt.Errorf("no registered repo with root %q (use `veska repo list` to see registered repos)", abs)
}

// looksLikeRepoID reports whether arg is plausibly a repo_id or short_id
// prefix - a non-empty hex-only string. Repo IDs are SHA-256 hex; even a
// 4-char prefix is uniquely identifying in practice. Filesystem paths almost
// always contain a non-hex character (`/`, `.`, `-`).
func looksLikeRepoID(arg string) bool {
	if arg == "" {
		return false
	}
	for _, c := range arg {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// OpenLocalDB opens the on-disk sqlite database with full migrations applied
// and returns a close function so the caller releases the WAL connection
// promptly. Used as the fallback path when the daemon is not running.
func OpenLocalDB() (*sql.DB, func(), error) {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	handle, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	return handle, func() { _ = handle.Close() }, nil
}

// daemonLogPath returns the daemon's structured-log path under the data root.
func daemonLogPath() string {
	return filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
}

// dialAddRepo sends eng_add_repo over the daemon's MCP unix socket. Returns
// the assigned repo_id on success. Errors are translated into context - a
// dial failure means "daemon not running" and the caller should fall back.
func dialAddRepo(ctx context.Context, rootPath string) (string, bool, error) {
	type result struct {
		RepoID            string `json:"repo_id"`
		ScanPending       bool   `json:"scan_pending"`
		AlreadyRegistered bool   `json:"already_registered"`
	}
	var r result
	if err := mcpclient.Call(ctx, "eng_add_repo", map[string]any{"root_path": rootPath}, &r); err != nil {
		return "", false, err
	}
	if r.RepoID == "" {
		return "", false, errors.New("daemon returned empty repo_id")
	}
	return r.RepoID, r.AlreadyRegistered, nil
}

// dialRemoveRepo sends eng_remove_repo over the daemon's MCP unix socket.
func dialRemoveRepo(ctx context.Context, repoID string) error {
	var r struct{}
	return mcpclient.Call(ctx, "eng_remove_repo", map[string]any{"repo_id": repoID}, &r)
}

// dialEngStatus probes whether the daemon socket is reachable. Used by
// applyBulkRemove to choose the daemon path vs direct DB without a separate
// guess.
func dialEngStatus(ctx context.Context) (any, error) {
	var resp any
	err := mcpclient.Call(ctx, "eng_get_status", map[string]any{}, &resp)
	return resp, err
}

// ScanProgressRow is the per-scan progress snapshot surfaced into repo
// list - phase ("walking" / "promoting") + files_seen - so a user can
// tell the sub-second walk from the long promotion phase that follows
// it.
type ScanProgressRow struct {
	Phase     string
	FilesSeen int
	StartedAt time.Time
}

// FetchScanProgress pulls scans_in_flight from eng_get_status and returns
// a map repo_id → progress. Best-effort: nil if the call fails or the
// daemon is too old to surface the fields.
func FetchScanProgress(ctx context.Context) map[string]ScanProgressRow {
	var status struct {
		ScansInFlight []struct {
			RepoID    string    `json:"repo_id"`
			Phase     string    `json:"phase"`
			FilesSeen int       `json:"files_seen"`
			StartedAt time.Time `json:"started_at"`
		} `json:"scans_in_flight"`
	}
	if err := mcpclient.Call(ctx, "eng_get_status", map[string]any{}, &status); err != nil {
		return nil
	}
	m := make(map[string]ScanProgressRow, len(status.ScansInFlight))
	for _, s := range status.ScansInFlight {
		m[s.RepoID] = ScanProgressRow{
			Phase:     s.Phase,
			FilesSeen: s.FilesSeen,
			StartedAt: s.StartedAt,
		}
	}
	return m
}
