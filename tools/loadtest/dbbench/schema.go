//go:build eval

package dbbench

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// SchemaStatements returns the ordered list of CREATE statements that the
// bench applies to a fresh DB. The bench owns its own minimal schema (a
// trimmed superset of what the six workloads touch) rather than importing
// the production migrations, so it can be applied identically through both
// database/sql drivers and zombiezen's sqlitex.
func SchemaStatements() ([]string, error) {
	entries, err := schemaFS.ReadDir("schema")
	if err != nil {
		return nil, fmt.Errorf("read schema dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		b, err := schemaFS.ReadFile("schema/" + n)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n, err)
		}
		// Split on ";" + newline - every statement in this dir ends with ";\n".
		for _, stmt := range splitStatements(string(b)) {
			s := strings.TrimSpace(stmt)
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func splitStatements(s string) []string {
	parts := strings.Split(s, ";\n")
	if len(parts) == 1 {
		return parts
	}
	// Re-append the ";" we lost on the split (only matters for executors that
	// require it; harmless for the rest).
	for i := range parts[:len(parts)-1] {
		parts[i] += ";"
	}
	return parts
}
