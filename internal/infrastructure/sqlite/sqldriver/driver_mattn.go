//go:build sqlite_mattn

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
// names (which differ from modernc's `_pragma=k=v` form).
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
