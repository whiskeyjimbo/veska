// Package sqldriver selects veska's SQLite driver at build time.
//
// Default build: modernc.org/sqlite (pure-Go, no cgo). This is production.
// Build with `-tags=sqlite_mattn` and `CGO_ENABLED=1` to swap in the cgo
// driver `github.com/mattn/go-sqlite3` (which additionally needs the
// `sqlite_fts5` tag, because veska's lexical fallback uses FTS5 virtual
// tables).
//
// The shim exists for solov2-jkgp: end-to-end measurement of whether
// swapping drivers moves real veska latency enough to justify taking on
// cgo + losing the CGO_ENABLED=0 cross-compile story. If the answer is
// no, this package stays and the mattn variant simply rusts; if yes, the
// build flag flips to default-on and modernc is eventually removed.
package sqldriver
