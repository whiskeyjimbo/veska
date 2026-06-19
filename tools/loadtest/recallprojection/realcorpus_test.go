// SPDX-License-Identifier: AGPL-3.0-only

package recallprojection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureModule is a minimal Go package: two documented funcs (one a
// method), one documented type, and one undocumented exported func.
const fixtureModule = `// Package sample is a fixture package.
package sample

// Greet returns a friendly greeting addressed to name.
func Greet(name string) string {
	return "hello " + name
}

func Loud(s string) string { return s + "!" }

// Counter accumulates a running integer total.
type Counter struct {
	total int
}

// Add increments the counter by the delta amount.
func (c *Counter) Add(delta int) {
	c.total += delta
}
`

func writeFixtureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(fixtureModule), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func nodeBySymbol(c ProjectionCorpus, symbol string) (ProjectionNode, bool) {
	for _, n := range c.Nodes {
		if n.Input.SymbolPath == symbol {
			return n, true
		}
	}
	return ProjectionNode{}, false
}

func TestBuildRealCorpus_DocumentedSymbolsBecomeQueriedClusters(t *testing.T) {
	c, err := BuildRealCorpus(writeFixtureModule(t))
	if err != nil {
		t.Fatalf("BuildRealCorpus: %v", err)
	}

	// Greet, Counter, Add are documented → 3 queried clusters; Loud is an
	// undocumented distractor → 4 nodes, a trailing distractor cluster.
	if len(c.CenterQueries) != 3 {
		t.Errorf("CenterQueries: got %d, want 3", len(c.CenterQueries))
	}
	if len(c.Nodes) != 4 {
		t.Errorf("Nodes: got %d, want 4", len(c.Nodes))
	}
	if c.Clusters != 4 {
		t.Errorf("Clusters: got %d, want 4 (3 documented + 1 distractor bucket)", c.Clusters)
	}

	// Truth is single-node per documented cluster, and every queried
	// cluster index is in range.
	truth := c.TruthByCluster()
	for cluster := range c.CenterQueries {
		if got := len(truth[cluster]); got != 1 {
			t.Errorf("cluster %d truth size: got %d, want 1", cluster, got)
		}
	}
}

func TestBuildRealCorpus_RealBodyAndDisjointQuery(t *testing.T) {
	c, err := BuildRealCorpus(writeFixtureModule(t))
	if err != nil {
		t.Fatalf("BuildRealCorpus: %v", err)
	}

	greet, ok := nodeBySymbol(c, "sample.Greet")
	if !ok {
		t.Fatal("sample.Greet not in corpus")
	}
	if greet.Input.Kind != "function" {
		t.Errorf("Greet kind: got %q, want function", greet.Input.Kind)
	}
	if !strings.Contains(greet.Input.Snippet, `return "hello " + name`) {
		t.Errorf("Greet snippet missing real body: %q", greet.Input.Snippet)
	}
	if !strings.Contains(greet.Input.Signature, "func Greet(name string) string") {
		t.Errorf("Greet signature wrong: %q", greet.Input.Signature)
	}
	// Snippet (code) and query (doc comment) must be disjoint - the
	// snippet must NOT carry the doc-comment prose.
	if strings.Contains(greet.Input.Snippet, "friendly greeting") {
		t.Errorf("Greet snippet leaked the doc comment: %q", greet.Input.Snippet)
	}
	if q := c.CenterQueries[greet.Cluster]; !strings.Contains(q, "friendly greeting") {
		t.Errorf("Greet query is not its doc comment: %q", q)
	}
}

func TestBuildRealCorpus_MethodAndDistractor(t *testing.T) {
	c, err := BuildRealCorpus(writeFixtureModule(t))
	if err != nil {
		t.Fatalf("BuildRealCorpus: %v", err)
	}

	add, ok := nodeBySymbol(c, "sample.Counter.Add")
	if !ok {
		t.Fatal("sample.Counter.Add not in corpus")
	}
	if add.Input.Kind != "method" {
		t.Errorf("Add kind: got %q, want method", add.Input.Kind)
	}

	// Loud is exported but undocumented → present as a distractor in a
	// cluster past every queried cluster, never a query.
	loud, ok := nodeBySymbol(c, "sample.Loud")
	if !ok {
		t.Fatal("undocumented sample.Loud should still be a distractor node")
	}
	if loud.Cluster < len(c.CenterQueries) {
		t.Errorf("Loud cluster %d should be >= %d (a non-queried distractor)",
			loud.Cluster, len(c.CenterQueries))
	}
}

func TestBuildRealCorpus_EmptyDirIsEmptyCorpus(t *testing.T) {
	c, err := BuildRealCorpus(t.TempDir())
	if err != nil {
		t.Fatalf("BuildRealCorpus on empty dir: %v", err)
	}
	if len(c.Nodes) != 0 || c.Clusters != 0 || len(c.CenterQueries) != 0 {
		t.Errorf("empty dir should yield an empty corpus, got %+v", c)
	}
}
