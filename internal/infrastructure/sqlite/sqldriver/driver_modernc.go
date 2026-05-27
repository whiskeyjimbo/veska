//go:build !sqlite_mattn

package sqldriver

import (
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

// Name is the database/sql driver name to pass to sql.Open.
const Name = "sqlite"

// Variant is a human-readable label for telemetry / RESULTS.md headers.
const Variant = "modernc"

// BuildDSN encodes WAL + foreign_keys + synchronous=NORMAL + busy_timeout
// onto dbPath as DSN-level connection pragmas. The encoding is driver-
// specific; see the mattn variant for its differing param names.
//
// dbPath may be a plain filesystem path or an existing `file:` DSN with
// query params (e.g. an in-memory shared-cache handle); the pragmas are
// appended either way.
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
	q.Add("_pragma", "journal_mode=WAL")
	q.Add("_pragma", "foreign_keys=on")
	q.Add("_pragma", "synchronous=NORMAL")
	q.Add("_pragma", fmt.Sprintf("busy_timeout=%d", busyTimeoutMS))
	return base + sep + q.Encode()
}
