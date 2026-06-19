// SPDX-License-Identifier: AGPL-3.0-only

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

// ErrAliasExists is returned when the alias name already points to a different
// repository and force is false.
var ErrAliasExists = errors.New("alias already exists for a different repo")

// ErrAliasNotFound is returned when the requested alias name is not registered.
var ErrAliasNotFound = errors.New("alias not found")

// ErrAliasInvalid is returned when the alias name fails validation checks
// because it contains whitespace, is empty, or shadows hex-based repository IDs.
var ErrAliasInvalid = errors.New("alias name is invalid")

// SetAlias creates or overwrites a repository alias. It is a no-op if the alias already
// points to the target repository; it returns ErrAliasExists if it points to a different
// repository, unless force is set to true.
func SetAlias(ctx context.Context, db *sql.DB, name, repoID string, force bool) error {
	if err := ValidateAliasName(name); err != nil {
		return err
	}

	var existing string
	err := db.QueryRowContext(ctx, `SELECT repo_id FROM repo_aliases WHERE name = ?`, name).Scan(&existing)
	switch {
	case err == sql.ErrNoRows:
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

// RemoveAlias deletes the specified alias from the database, returning ErrAliasNotFound
// if no alias with the given name existed.
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

// LookupAlias resolves an alias name to its corresponding repository ID,
// returning a boolean flag indicating whether the alias was found.
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

// AliasesByRepoID returns a map of all repository IDs to their sorted alias names.
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

// AliasesForRepo retrieves the sorted list of aliases registered for a single repository ID.
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

// ValidateAliasName ensures that an alias name does not contain whitespace, is not empty,
// and does not shadow repository hex ID prefixes (which must be at least 4 hex characters).
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

// isHexPrefix reports whether the string consists only of hexadecimal characters
// and is at least 4 characters long.
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

// SuggestAliasNames returns suggested primary and fallback alias names for a repository.
// For URL-registered repositories, the primary suggestion is the repository basename and
// the fallback is the owner-basename combination. For local paths, the primary is the directory
// basename and there is no fallback.
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

// parseURLOwnerName parses a Git URL to extract the repository owner and name,
// stripping any ".git" suffix from the name segment.
func parseURLOwnerName(canonicalURL string) (owner, name string) {
	u, err := url.Parse(canonicalURL)
	if err != nil {
		return "", ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Drop empty path segments.
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
