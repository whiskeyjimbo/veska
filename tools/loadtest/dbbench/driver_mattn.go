//go:build eval && cgo && sqlite_fts5

package dbbench

import _ "github.com/mattn/go-sqlite3"

func init() {
	Register("mattn", func() Bench { return newSQLBench("mattn", "sqlite3") })
}
