//go:build eval

package dbbench

import _ "modernc.org/sqlite"

func init() {
	Register("modernc", func() Bench { return newSQLBench("modernc", "sqlite") })
}
