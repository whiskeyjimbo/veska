// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrInvalidURL is returned when the raw input cannot be parsed as a recognized Git URL format.
var ErrInvalidURL = errors.New("invalid git url")

// CanonicalURL returns the canonicalized form of a Git URL. It handles standard Git protocols
// (http, https, ssh, git, file) and scp-like SSH formats, converting them to a standard HTTPS URL representation
// while preserving path casing and ports, stripping user info, and removing trailing slashes and ".git" suffixes.
func CanonicalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidURL
	}

	scheme, rest, ok := splitScheme(raw)
	if !ok {
		// Parse scp-like URLs format of [user@]host:path by ensuring a colon separates the host from the path.
		host, path, sep := strings.Cut(raw, ":")
		if !sep || host == "" || path == "" || strings.Contains(host, "/") {
			return "", ErrInvalidURL
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if host == "" {
			return "", ErrInvalidURL
		}
		return normaliseURL("https", host, "/"+path), nil
	}

	switch scheme {
	case "https", "http", "ssh", "git":
		// Authority and path extraction.
		authority, path, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = authority[at+1:]
		}
		if authority == "" {
			return "", ErrInvalidURL
		}
		if path != "" {
			path = "/" + path
		}
		return normaliseURL("https", authority, path), nil
	case "file":
		// File URLs remain prefixed with file://, stripping the authority segment for canonicality.
		_, path, _ := strings.Cut(rest, "/")
		if path == "" {
			return "", ErrInvalidURL
		}
		path = "/" + strings.TrimSuffix(path, "/")
		path = strings.TrimSuffix(path, ".git")
		return "file://" + path, nil
	}
	return "", ErrInvalidURL
}

// splitScheme extracts the protocol scheme from the start of a URL, returning false if no "://" separator is present.
func splitScheme(raw string) (scheme, rest string, ok bool) {
	idx := strings.Index(raw, "://")
	if idx <= 0 {
		return "", "", false
	}
	scheme = strings.ToLower(raw[:idx])
	for _, c := range scheme {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return "", "", false
		}
	}
	return scheme, raw[idx+3:], true
}

// normaliseURL standardizes URL components by lowercasing the host and stripping trailing slashes or ".git" suffixes.
func normaliseURL(scheme, authority, path string) string {
	host, port, hasPort := strings.Cut(authority, ":")
	host = strings.ToLower(host)
	if hasPort {
		authority = host + ":" + port
	} else {
		authority = host
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	return scheme + "://" + authority + path
}

// TrackedClonePath returns the destination directory path for a tracked repository clone using a shortened hash of its canonical URL.
func TrackedClonePath(veskaHome, canonicalURL string) string {
	return filepath.Join(veskaHome, "repos", DerivedRepoIDFromURL(canonicalURL)[:16])
}

// LookupByCanonicalURL retrieves a registered repository record by matching its canonicalized URL.
func LookupByCanonicalURL(ctx context.Context, db *sql.DB, urlOrCanonical string) (Record, bool, error) {
	canonical, err := CanonicalURL(urlOrCanonical)
	if err != nil {
		return Record{}, false, err
	}
	var (
		rec     Record
		branch  sql.NullString
		lastSHA sql.NullString
	)
	err = db.QueryRowContext(ctx,
		`SELECT repo_id, root_path, active_branch, last_promoted_sha, kind
		 FROM repos WHERE canonical_url = ?`,
		canonical,
	).Scan(&rec.RepoID, &rec.RootPath, &branch, &lastSHA, &rec.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("lookup by canonical_url: %w", err)
	}
	rec.ActiveBranch = branch.String
	rec.LastPromotedSHA = lastSHA.String
	return rec, true, nil
}

// PromoteEphemeralToTracked changes the repository's status from 'ephemeral' to 'tracked' in the database.
func PromoteEphemeralToTracked(ctx context.Context, db *sql.DB, repoID string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE repos SET kind = 'tracked', prompted_at = ? WHERE repo_id = ? AND kind = 'ephemeral'`,
		nowUnix(), repoID,
	)
	if err != nil {
		return fmt.Errorf("promote ephemeral: %w", err)
	}
	return nil
}

// MarkPromptDeclined updates the prompt timestamp for an ephemeral repository without promoting it, suppressing future prompts.
func MarkPromptDeclined(ctx context.Context, db *sql.DB, repoID string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE repos SET prompted_at = ? WHERE repo_id = ? AND kind = 'ephemeral'`,
		nowUnix(), repoID,
	)
	if err != nil {
		return fmt.Errorf("mark prompt declined: %w", err)
	}
	return nil
}

// TouchEphemeral updates the last accessed timestamp for an ephemeral repository, facilitating LRU eviction.
// Touches the timestamp to prevent immediate eviction.
func TouchEphemeral(ctx context.Context, db *sql.DB, repoID string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE repos SET last_accessed_at = ? WHERE repo_id = ? AND kind = 'ephemeral'`,
		nowUnix(), repoID,
	)
	if err != nil {
		return fmt.Errorf("touch ephemeral: %w", err)
	}
	return nil
}

// nowUnix acts as a pluggable time source for unit tests.
var nowUnix = func() int64 { return time.Now().Unix() }

// SetCanonicalURL updates the canonical URL for the specified repository ID.
func SetCanonicalURL(ctx context.Context, db *sql.DB, repoID, urlOrCanonical string) error {
	canonical, err := CanonicalURL(urlOrCanonical)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE repos SET canonical_url = ? WHERE repo_id = ?`,
		canonical, repoID,
	)
	if err != nil {
		return fmt.Errorf("set canonical_url: %w", err)
	}
	return nil
}

// AddFromURL clones a remote Git repository to the tracked clones directory and registers it.
// The operation is idempotent; if the canonicalized URL is already registered, it returns the
// existing repository ID without re-cloning. In the event of a partial registration failure,
// the cloned directory is cleaned up.
func AddFromURL(ctx context.Context, db *sql.DB, veskaHome, rawURL string, progressW io.Writer) (string, bool, error) {
	canonical, err := CanonicalURL(rawURL)
	if err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}

	if existing, ok, err := LookupByCanonicalURL(ctx, db, canonical); err != nil {
		return "", false, err
	} else if ok {
		return existing.RepoID, true, nil
	}

	dest := TrackedClonePath(veskaHome, canonical)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}
	// Remove any existing directory at the destination to prevent clone failures from stale state.
	if _, statErr := os.Stat(dest); statErr == nil {
		_ = os.RemoveAll(dest)
	}

	if _, err := Clone(ctx, canonical, dest, progressW); err != nil {
		return "", false, err
	}

	id, existed, err := Add(ctx, db, dest)
	if err != nil {
		// Remove the cloned directory to ensure clean retries when registration fails.
		_ = os.RemoveAll(dest)
		return "", false, err
	}
	if err := SetCanonicalURL(ctx, db, id, canonical); err != nil {
		_ = os.RemoveAll(dest)
		_, _ = db.ExecContext(ctx, `DELETE FROM repos WHERE repo_id = ?`, id)
		return "", false, err
	}
	return id, existed, nil
}

// Clone runs `git clone --depth=1` to clone a repository to the destination directory,
// writing output messages directly to the progress writer.
func Clone(ctx context.Context, url, destDir string, progressW io.Writer) (string, error) {
	if progressW == nil {
		progressW = io.Discard
	}
	// We omit the --progress flag so Git automatically detects if it should produce progress output based on TTY presence.
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", url, destDir)
	var captured bytes.Buffer
	cmd.Stderr = io.MultiWriter(progressW, &captured)
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(captured.String())
		if stderr == "" {
			return "", fmt.Errorf("git clone %s: %w", url, err)
		}
		return "", fmt.Errorf("git clone %s: %w: %s", url, err, stderr)
	}
	return destDir, nil
}
