// Package config provides configuration constants and path helpers.
package config

import (
	"os"
	"path/filepath"
)

// veskaHome returns the root Veska data directory.
// It uses $VESKA_HOME when set; otherwise ~/.veska.
// Falls back to ".veska" when the home directory cannot be determined.
func veskaHome() string {
	if dir := os.Getenv("VESKA_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".veska"
	}
	return filepath.Join(home, ".veska")
}

// DefaultVectorDir returns the default directory for persisted vector index files.
// It resolves to ~/.veska using os.UserHomeDir.
// If the home directory cannot be determined, it falls back to ".veska" relative
// to the working directory.
func DefaultVectorDir() string {
	return veskaHome()
}

// CLISockPath returns the Unix-domain socket path the CLI uses to reach the daemon.
// It resolves to $VESKA_HOME/cli.sock, where VESKA_HOME defaults to ~/.veska.
func CLISockPath() string {
	return filepath.Join(veskaHome(), "cli.sock")
}

// MCPSockPath returns the path of the MCP Unix socket (~/.veska/mcp.sock).
// Respects VESKA_HOME env var.
func MCPSockPath() string {
	return filepath.Join(veskaHome(), "mcp.sock")
}

// DefaultOSVCacheDir returns the default directory for the OSV advisory cache.
// It resolves to $VESKA_HOME/cache/osv, where VESKA_HOME defaults to ~/.veska.
func DefaultOSVCacheDir() string {
	return filepath.Join(veskaHome(), "cache", "osv")
}

// DefaultBackupDir returns the directory `veska backup create` writes to:
// $VESKA_HOME/backups. Co-located with the rest of veska's
// state so a single `rm -rf $VESKA_HOME` clears everything - the original
// ~/.veska-backups layout left tarballs orphaned after a data wipe.
// Backward compat: LegacyBackupDir returns the prior path; callers that
// read backups (list, restore) should fall back to it when the canonical
// dir is empty.
func DefaultBackupDir() string {
	return filepath.Join(veskaHome(), "backups")
}

// LegacyBackupDir returns the pre-n57f backup location (~/.veska-backups).
// It is only used as a read-side fallback so an existing user upgrading
// keeps access to backups they took before this change. New writes always
// go to DefaultBackupDir. Returns ("", false) when the user's home
// directory cannot be resolved.
func LegacyBackupDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".veska-backups"), true
}

// CacheDir returns the root directory for ephemeral / evictable Veska data
// Precedence:
//  1. $VESKA_CACHE_HOME (explicit override; useful for CI / containers
//     that want cache colocated with data)
//  2. $XDG_CACHE_HOME/veska (XDG-compliant default)
//  3. ~/.cache/veska (fallback when XDG_CACHE_HOME is unset)
//
// Distinct from veskaHome: VESKA_HOME holds authoritative data (the
// graph DB, sockets, logs) that survives `rm -rf ~/.cache`; CacheDir
// holds regenerable cache the user can wipe safely. The cache-tier
// kxo5 design routes ephemeral repo clones here so a user clearing
// ~/.cache only invalidates re-cloneable state.
func CacheDir() string {
	if dir := os.Getenv("VESKA_CACHE_HOME"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "veska")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "veska")
	}
	return filepath.Join(".cache", "veska")
}

// RepoCachePath returns the on-disk path for an ephemeral, URL-cloned repo
// identified by its URL-derived repo_id. kxo5.6 is the
// first call site; until then this helper has no production consumers
// other than the CacheDir composition.
func RepoCachePath(repoID string) string {
	return filepath.Join(CacheDir(), "repos", repoID)
}
