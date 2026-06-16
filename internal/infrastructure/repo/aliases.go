package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// ErrAliasExists is returned by SetAlias when name already points at a
// different repo and force is false. Callers surface it as a hint to retry
// with --force.
var ErrAliasExists = errors.New("alias already exists for a different repo")

// ErrAliasNotFound is returned by RemoveAlias / LookupAlias for an unknown
// alias name.
var ErrAliasNotFound = errors.New("alias not found")

// ErrAliasInvalid is returned by SetAlias when name fails validation
// (empty, whitespace, or looks like a repo_id / short_id prefix that would
// shadow the prefix-resolution step in the resolver).
var ErrAliasInvalid = errors.New("alias name is invalid")

// SetAlias creates or replaces an alias. When the name already points at
// repoID the call is a no-op; when it points elsewhere ErrAliasExists is
// returned unless force is true.
func SetAlias(ctx context.Context, db *sql.DB, name, repoID string, force bool) error {
	if err := ValidateAliasName(name); err != nil {
		return err
	}

	var existing string
	err := db.QueryRowContext(ctx, `SELECT repo_id FROM repo_aliases WHERE name = ?`, name).Scan(&existing)
	switch {
	case err == sql.ErrNoRows:
		// insert below
	case err != nil:
		return fmt.Errorf("lookup alias: %w", err)
	case existing == repoID:
		return nil
	case !force:
		return fmt.Errorf("%w: %q -> %s", ErrAliasExists, name, existing)
	}

	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`INSERT INTO repo_aliases (name, repo_id) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET repo_id = excluded.repo_id`,
		name, repoID,
	); err != nil {
		return fmt.Errorf("set alias: %w", err)
	}
	return nil
}

// RemoveAlias drops name. Returns ErrAliasNotFound if no row matched so a
// CLI caller can distinguish typo-on-removal from a successful delete.
func RemoveAlias(ctx context.Context, db *sql.DB, name string) error {
	res, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM repo_aliases WHERE name = ?`, name,
	)
	if err != nil {
		return fmt.Errorf("remove alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrAliasNotFound, name)
	}
	return nil
}

// LookupAlias returns the repo_id name points at. The bool reports whether
// a row was found so callers can distinguish "no such alias" from an error.
func LookupAlias(ctx context.Context, db *sql.DB, name string) (string, bool, error) {
	var repoID string
	err := db.QueryRowContext(ctx, `SELECT repo_id FROM repo_aliases WHERE name = ?`, name).Scan(&repoID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup alias: %w", err)
	}
	return repoID, true, nil
}

// AliasesByRepoID returns a map repo_id -> sorted alias names. Empty repos
// are absent from the map (callers default to a nil slice).
func AliasesByRepoID(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT repo_id, name FROM repo_aliases ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]string)
	for rows.Next() {
		var repoID, name string
		if err := rows.Scan(&repoID, &name); err != nil {
			return nil, fmt.Errorf("scan alias row: %w", err)
		}
		out[repoID] = append(out[repoID], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alias rows: %w", err)
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out, nil
}

// AliasesForRepo returns the sorted alias list for a single repo. A repo
// with no aliases yields a nil slice and a nil error.
func AliasesForRepo(ctx context.Context, db *sql.DB, repoID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM repo_aliases WHERE repo_id = ? ORDER BY name`,
		repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("aliases for repo: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan alias row: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alias rows: %w", err)
	}
	return out, nil
}

// ValidateAliasName rejects names that would either silently shadow the
// resolver's higher-precedence steps (full repo_id, 12-char short_id, hex
// prefix) or be unusable as a CLI argument.
// Specifically: empty/whitespace, contains whitespace, or is hex-only and
// >= the minimum prefix length the resolver accepts (4 chars). The latter
// is what looksLikeRepoID would catch — we duplicate the check here rather
// than import a CLI helper into the repo package.
func ValidateAliasName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrAliasInvalid)
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("%w: whitespace", ErrAliasInvalid)
	}
	if isHexPrefix(name) {
		return fmt.Errorf("%w: %q looks like a repo_id (hex only, >= 4 chars)", ErrAliasInvalid, name)
	}
	return nil
}

// isHexPrefix reports whether s is hex-only and long enough to be accepted
// as a repo_id prefix by the resolver. Kept package-private; the only
// caller is ValidateAliasName.
func isHexPrefix(s string) bool {
	if len(s) < 4 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// SuggestAliasNames returns (primary, fallback) name candidates for a
// freshly added repo. The CLI's auto-suggest prompt offers primary, then
// falls back to fallback when primary collides with an existing alias.
// For a URL-registered repo: primary is the repo basename ("bar" for
// https://github.com/foo/bar), fallback is "<owner>-<name>" ("foo-bar").
// For a path-registered repo with no canonical URL: primary is the
// directory basename, fallback is "" (caller skips on collision).
func SuggestAliasNames(canonicalURL, rootPath string) (primary, fallback string) {
	if canonicalURL != "" {
		owner, name := parseURLOwnerName(canonicalURL)
		if name != "" {
			fallback = owner + "-" + name
			if owner == "" {
				fallback = ""
			}
			return name, fallback
		}
	}
	if rootPath != "" {
		return path.Base(rootPath), ""
	}
	return "", ""
}

// parseURLOwnerName extracts the trailing two path segments of a git URL
// as (owner, name). Drops a ".git" suffix on name. Returns ("", "") when
// the URL is unparseable; ("", name) when only one segment is available
// (e.g. a self-hosted single-tenant URL).
func parseURLOwnerName(canonicalURL string) (owner, name string) {
	u, err := url.Parse(canonicalURL)
	if err != nil {
		return "", ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Drop empty segments.
	out := segs[:0]
	for _, s := range segs {
		if s != "" {
			out = append(out, s)
		}
	}
	segs = out
	switch len(segs) {
	case 0:
		return "", ""
	case 1:
		return "", strings.TrimSuffix(segs[0], ".git")
	default:
		last := len(segs) - 1
		return segs[last-1], strings.TrimSuffix(segs[last], ".git")
	}
}
