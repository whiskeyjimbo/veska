// SPDX-License-Identifier: AGPL-3.0-only

// Package sqldriver pins veska's SQLite driver.
// veska uses github.com/mattn/go-sqlite3 (cgo, with the sqlite_fts5 build
// tag so the lexical-fallback FTS5 virtual tables work). cgo is required
// regardless because tree-sitter is cgo, so the historical pure-Go
// (modernc) opt-in had no remaining value and was removed.
package sqldriver

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Name is the database/sql driver name to pass to sql.Open. It is a veska-owned
// registration (not the bare "sqlite3" mattn registers) so a ConnectHook can run
// the connection-scoped performance pragmas mattn's DSN parser does not accept -
// see connectPragmas.
const Name = "sqlite3_veska"

// Variant is a human-readable label for telemetry / RESULTS.md headers.
const Variant = "mattn"

// connectPragmas are performance pragmas applied to every connection via the
// ConnectHook. They are connection-scoped, so they must run per-connection
// rather than once on a migration handle: the runtime pools (pools.go) open
// their own connections and never call applyPragmas. temp_store is not among
// mattn's DSN-recognized parameters, so the DSN cannot carry it; the hook is the
// uniform place for both.
//   - cache_size=-16000: ~16 MiB page cache (negative = KiB), up from the 2 MiB default.
//   - temp_store=MEMORY: sort/group/temp b-trees in RAM instead of temp files,
//     which helps the GROUP BY structural-check queries.
//
// These are connection-config hygiene; no workload A/B was run, since veska's
// working set is small and warm and no single operation gates them. mmap_size
// was deliberately left out - its benefit is data-size-dependent and not
// measurable on the current working set.
var connectPragmas = []string{
	"PRAGMA cache_size = -16000",
	"PRAGMA temp_store = MEMORY",
}

func init() {
	sql.Register(Name, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			for _, p := range connectPragmas {
				if _, err := conn.Exec(p, nil); err != nil {
					return fmt.Errorf("sqldriver: %q: %w", p, err)
				}
			}
			return nil
		},
	})
}

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
