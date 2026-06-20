// SPDX-License-Identifier: AGPL-3.0-only

// Package treesitter provides query_runner, a wrapper over sitter.QueryCursor that executes
// tree-sitter queries. It compiles queries once per process via sync.Once, minimizes CGO
// crossings by iterating within C, and exposes captures by name for easier processing.
package treesitter

import (
	"embed"
	"fmt"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

//go:embed queries
var queryFS embed.FS

// loadedQuery caches a compiled sitter.Query. It uses sync.Once to compile the query
// on demand and prevent race conditions.
type loadedQuery struct {
	once sync.Once
	q    *sitter.Query
	err  error
}

var (
	queryCacheMu sync.Mutex
	queryCache   = map[string]*loadedQuery{}
)

// compileEmbeddedQuery loads a tree-sitter query file from the embedded filesystem and
// compiles it. It caches the compiled query for subsequent requests. Compiling a query
// failure returns a persistent error.
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

// queryMatch holds the named captures extracted from a tree-sitter query match.
type queryMatch struct {
	captures map[string]*sitter.Node
}

// node returns the captured node for a given name, or nil if the capture was not matched.
func (m queryMatch) node(name string) *sitter.Node {
	return m.captures[name]
}

// runQuery executes a query against a root node and returns the query matches in document
// order. Anonymous captures (captures without a `@name` suffix) are skipped.
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
