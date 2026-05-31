package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// shared.go collects the small cross-command helpers that several cobra
// subcommands lean on (daemon liveness, backup-directory resolution, and
// byte formatting). They previously lived in whichever command file happened
// to define them first (daemonRunning/backup-dir in restore.go, a byte
// formatter duplicated across backup.go and savings.go); centralising them
// keeps the call sites discoverable and the formatting consistent.

// daemonRunning reports whether the veska daemon is up by dialing its CLI
// Unix socket. A successful connection means the daemon is listening.
func daemonRunning() bool {
	conn, err := net.DialTimeout("unix", config.CLISockPath(), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// resolveBackupReadDir returns the directory to READ backups from. Prefer
// the canonical $VESKA_HOME/backups; fall back to the legacy
// ~/.veska-backups when the canonical dir is missing or has no tarballs,
// so users upgrading still see backups they took under the old layout
// (solov2-n57f).
func resolveBackupReadDir() (string, error) {
	canon := config.DefaultBackupDir()
	if hasBackupTarballs(canon) {
		return canon, nil
	}
	if legacy, ok := config.LegacyBackupDir(); ok && hasBackupTarballs(legacy) {
		return legacy, nil
	}
	// No tarballs anywhere — return the canonical path so error messages
	// point at the location new writes will land.
	return canon, nil
}

// hasBackupTarballs reports whether dir contains at least one *.tar.gz.
// Used by resolveBackupReadDir to choose between the canonical and legacy
// locations.
func hasBackupTarballs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			return true
		}
	}
	return false
}

// humanBytes renders a byte count in the largest base-1024 unit that keeps
// the numeric part under 1024 ("873B", "1.2KB", "1.2MB", "1.2GB"). The
// output stays narrow so it fits comfortably in tabular and 80-column
// terminal output across the backup and savings commands.
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(k*k*k))
	}
}

// resolveRepoFromCWD asks the daemon (via eng_get_current_repo) which repo
// the caller's cwd belongs to. Used by CLI wrappers (symbol, context, ...)
// to bridge the gap when the daemon has multiple repos registered and the
// user hasn't passed --repo. Empty string + no error means "couldn't
// resolve"; the caller should still pass the request through and let the
// daemon's "repo_id is required" error surface (solov2-zukc).
func resolveRepoFromCWD(ctx context.Context) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil // cwd lookup failed; don't fail the whole command
	}
	var res struct {
		Repo struct {
			RepoID   string `json:"repo_id"`
			RootPath string `json:"root_path"`
		} `json:"repo"`
	}
	if err := mcpclient.Call(ctx, "eng_get_current_repo", map[string]any{"cwd": cwd}, &res); err != nil {
		// Daemon down or no match — caller falls through with no auto-resolve.
		return "", nil
	}
	return res.Repo.RepoID, nil
}

// autoResolveRepo wraps resolveRepoFromCWD with a stderr breadcrumb so the
// user is never surprised when --repo defaulted to a repo other than the
// one they were thinking of. Multi-repo silent fallback was the
// #1 first-impression bug in the junior-journey walk-through (solov2-dqwh).
// errOut may be nil to suppress the hint (e.g. JSON-output paths where a
// stray stderr line could clutter pipelines — callers there pay the
// no-hint cost knowingly). Shared by the deps, findings, and symbol command
// families.
func autoResolveRepo(ctx context.Context, errOut io.Writer) string {
	rid, _ := resolveRepoFromCWD(ctx)
	if rid == "" {
		return ""
	}
	// Only emit the hint when we know there's more than one repo to choose
	// between — solo-repo users don't need the noise.
	var list struct {
		Repos []struct {
			RepoID   string `json:"repo_id"`
			ShortID  string `json:"short_id"`
			RootPath string `json:"root_path"`
		} `json:"repos"`
	}
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &list); err == nil && len(list.Repos) > 1 && errOut != nil {
		short, root := rid[:12], ""
		for _, rec := range list.Repos {
			if rec.RepoID == rid {
				if rec.ShortID != "" {
					short = rec.ShortID
				}
				root = rec.RootPath
				break
			}
		}
		fmt.Fprintf(errOut, "veska: scoped to repo %s (%s); pass --repo to override\n", short, root)
	}
	return rid
}
