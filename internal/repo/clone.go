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

// ErrInvalidURL is returned by CanonicalURL when raw cannot be parsed as a
// recognised git URL form (https, http, ssh://, git://, or scp-like
// [user@]host:path).
var ErrInvalidURL = errors.New("invalid git url")

// CanonicalURL returns the canonical form of a git URL used as the alias
// key for repo collision-resolution and as the input to DerivedRepoIDFromURL
// (solov2-kxo5.1).
//
// Rules:
//   - SSH scp-like form ([user@]host:path) is rewritten to https://host/path
//   - ssh:// and git:// schemes are rewritten to https://
//   - Host is lowercased; user-info is dropped
//   - Trailing .git on the path is stripped
//   - Trailing slash on the path is stripped
//   - Port (if present) is preserved
//   - Path case is preserved (some forges are case-sensitive)
//
// Anything that doesn't look like a URL at all returns ErrInvalidURL.
func CanonicalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidURL
	}

	scheme, rest, ok := splitScheme(raw)
	if !ok {
		// scp-like: [user@]host:path. Must have a ':' separating host
		// from path, and the segment before ':' must not contain '/'.
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
		// rest begins with the authority: [user@]host[:port][/path]
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
		// file URLs map to absolute paths and stay as file://; .git strip
		// + trailing-slash strip still apply. The authority is conventionally
		// empty or "localhost" and is stripped for canonicality.
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

// splitScheme separates the leading scheme from raw and returns the
// remainder after "://". Returns ok=false if raw has no "scheme://" prefix.
func splitScheme(raw string) (scheme, rest string, ok bool) {
	idx := strings.Index(raw, "://")
	if idx <= 0 {
		return "", "", false
	}
	scheme = strings.ToLower(raw[:idx])
	for _, c := range scheme {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
			return "", "", false
		}
	}
	return scheme, raw[idx+3:], true
}

// normaliseURL applies the host-lowercase, .git-strip, trailing-slash-strip
// rules and assembles the canonical string. authority may include :port.
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

// TrackedClonePath returns the on-disk root for a tracked URL-cloned repo
// (solov2-kxo5.3). The first 16 hex characters of the URL-derived id give
// a collision-free directory name without the unreadable 64-char form.
func TrackedClonePath(veskaHome, canonicalURL string) string {
	return filepath.Join(veskaHome, "repos", DerivedRepoIDFromURL(canonicalURL)[:16])
}

// LookupByCanonicalURL returns the registered repo whose canonical_url
// column matches the canonicalised URL. The needle is re-canonicalised
// inside so callers can pass either the raw or canonical form (solov2-kxo5.4
// adds the same helper signature; landed here because kxo5.3 needs it for
// the "already registered" short-circuit).
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

// TouchEphemeral bumps last_accessed_at to now for repoID, but only when
// the row is kind='ephemeral'. Tracked rows are skipped silently — they
// are not subject to LRU eviction so the column is meaningless for them
// (solov2-kxo5.8). The combined WHERE clause is the gate; callers do not
// need to check kind themselves.
//
// Safe to call multiple times within a single query; the UPDATE is
// idempotent and the second write is a no-op cost-wise.
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

// nowUnix is a seam for tests; the actual time source lives here so
// TouchEphemeral can be tested without injecting a clock.
var nowUnix = func() int64 { return time.Now().Unix() }

// SetCanonicalURL writes (or rewrites) the canonical_url column for the
// given repoID. The value is canonicalised inside so callers don't have to
// remember which form to pass.
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

// AddFromURL clones a remote git URL into the tracked clones tier and
// registers it like a normal `repo add <path>` (solov2-kxo5.3).
//
//   - veskaHome is the root for the on-disk clone (typically $VESKA_HOME)
//   - progressW receives `git clone --progress` output; pass nil to discard
//
// The function is idempotent: if a row already exists with the same
// canonicalised URL, it returns that row's id with existed=true and
// performs no clone. A partial-clone failure (clone succeeded but Add or
// SetCanonicalURL failed) leaves no orphaned row; the cloned directory
// is removed so a retry starts clean.
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
	// git clone refuses to write into an existing directory; if a stale
	// clone fragment is sitting at dest from a prior failed run, clear it
	// before retrying so the user is not stuck with an unrecoverable repo.
	if _, statErr := os.Stat(dest); statErr == nil {
		_ = os.RemoveAll(dest)
	}

	if _, err := Clone(ctx, canonical, dest, progressW); err != nil {
		return "", false, err
	}

	id, existed, err := Add(ctx, db, dest)
	if err != nil {
		// Roll back the clone so a retry can succeed; the row didn't
		// land so leaving the dir would just confuse `repo add` next time.
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

// Clone shells out to `git clone --depth=1 --progress url destDir`, streaming
// git's stderr (which carries --progress lines) to progressW so callers can
// render a live indicator. On failure the captured stderr is included
// verbatim in the returned error — never swallowed or paraphrased — so a
// permission/auth/404 diagnosis is obvious from one error string.
//
// destDir must be a path that does not yet exist (git clone refuses to
// clone into an existing non-empty directory). The returned path equals
// destDir on success.
func Clone(ctx context.Context, url, destDir string, progressW io.Writer) (string, error) {
	if progressW == nil {
		progressW = io.Discard
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--progress", url, destDir)
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
