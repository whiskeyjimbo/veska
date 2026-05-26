// Package treesitter — query_runner is the thin wrapper over
// sitter.QueryCursor that the .scm-driven extractors use (solov2-1yev).
//
// Goals:
//   - One CGO crossing per match (vs per node in the legacy recursive
//     walkers); the cursor itself iterates inside the C extension.
//   - Capture results as a map[name]*Node so extractors can pull
//     "@function.name" directly without index gymnastics.
//   - Compile each query once per process via sync.Once so a hot parse
//     path doesn't re-compile S-expressions every call.
package treesitter

import (
	"embed"
	"fmt"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

//go:embed queries
var queryFS embed.FS

// loadedQuery caches one compiled *sitter.Query keyed by (language,
// query-path). Compilation is non-trivial (CGO call + S-expression
// parse) and the result is immutable, so a process-lifetime cache is
// the right scope. Each cache entry resolves under a single sync.Once
// so concurrent parses don't race the compile.
type loadedQuery struct {
	once sync.Once
	q    *sitter.Query
	err  error
}

var (
	queryCacheMu sync.Mutex
	queryCache   = map[string]*loadedQuery{}
)

// compileEmbeddedQuery loads queries/<lang>/<name>.scm from the embed
// FS, compiles it against lang, and memoises the result. Returns the
// same *sitter.Query on repeat calls. A nil query and a non-nil error
// indicate a permanent failure (missing file, parse error); callers
// should fail loudly — a missing query is a packaging bug, not a
// runtime condition to recover from.
func compileEmbeddedQuery(lang *sitter.Language, langName, queryName string) (*sitter.Query, error) {
	key := langName + "/" + queryName
	queryCacheMu.Lock()
	entry, ok := queryCache[key]
	if !ok {
		entry = &loadedQuery{}
		queryCache[key] = entry
	}
	queryCacheMu.Unlock()

	entry.once.Do(func() {
		path := "queries/" + langName + "/" + queryName + ".scm"
		data, err := queryFS.ReadFile(path)
		if err != nil {
			entry.err = fmt.Errorf("query_runner: read %s: %w", path, err)
			return
		}
		q, err := sitter.NewQuery(data, lang)
		if err != nil {
			entry.err = fmt.Errorf("query_runner: compile %s: %w", path, err)
			return
		}
		entry.q = q
	})
	return entry.q, entry.err
}

// queryMatch is the per-match capture envelope handed to extractors.
// Each entry maps the capture name (e.g. "function.name") to the
// underlying tree-sitter node. A capture absent from the match — for
// example an optional capture in an alternative branch — simply isn't
// in the map, so extractors check with `node, ok := m["..."]`.
type queryMatch struct {
	captures map[string]*sitter.Node
}

// node returns the captured node for name, or nil when absent.
// Convenience for extractors that don't want to use the two-value form.
func (m queryMatch) node(name string) *sitter.Node {
	return m.captures[name]
}

// runQuery executes q against root and returns one queryMatch per
// tree-sitter pattern match, in document order. The pattern's named
// captures populate each match's map. Anonymous captures (no @name)
// are silently dropped — they're useful for structural anchors in the
// pattern but rarely needed Go-side.
//
// Returns nil when the query has no matches. Callers iterate the slice
// and emit nodes/edges; the cursor's underlying CGO state is released
// before this function returns.
func runQuery(q *sitter.Query, root *sitter.Node) []queryMatch {
	if q == nil || root == nil {
		return nil
	}
	cursor := sitter.NewQueryCursor()
	defer cursor.Close()
	cursor.Exec(q, root)

	var out []queryMatch
	for {
		m, ok := cursor.NextMatch()
		if !ok {
			break
		}
		// Resolve each capture's name via Query.CaptureNameForId.
		caps := make(map[string]*sitter.Node, len(m.Captures))
		for _, c := range m.Captures {
			name := q.CaptureNameForId(c.Index)
			if name == "" {
				continue
			}
			// On capture-name conflicts (same name used twice in one
			// pattern), the last one wins. Our queries are written so
			// distinct subtree captures get distinct names, so this is
			// a defensive default rather than a load-bearing rule.
			caps[name] = c.Node
		}
		out = append(out, queryMatch{captures: caps})
	}
	return out
}
