// SPDX-License-Identifier: AGPL-3.0-only

// Package sqldriver pins veska's SQLite driver.
// veska uses github.com/mattn/go-sqlite3 (cgo, with the sqlite_fts5 build
// tag so the lexical-fallback FTS5 virtual tables work). cgo is required
// regardless because tree-sitter is cgo, so the historical pure-Go
// (modernc) opt-in had no remaining value and was removed.
package sqldriver

import (
	"fmt"
	"net/url"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// Name is the database/sql driver name to pass to sql.Open.
const Name = "sqlite3"

// Variant is a human-readable label for telemetry / RESULTS.md headers.
const Variant = "mattn"

// BuildDSN encodes WAL + foreign_keys + synchronous=NORMAL + busy_timeout
// onto dbPath as DSN-level connection pragmas using mattn's parameter
// names.
func BuildDSN(dbPath string, busyTimeoutMS int) string {
	base := dbPath
	if !strings.HasPrefix(base, "file:") {
		base = "file:" + base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	q := url.Values{}
	q.Set("_journal", "WAL")
	q.Set("_fk", "true")
	q.Set("_sync", "NORMAL")
	q.Set("_busy_timeout", fmt.Sprintf("%d", busyTimeoutMS))
	return base + sep + q.Encode()
}
