package coverage

// Frozen self-test: the guard against fixture drift. It re-indexes the golden
// fixture through the real cold-scan pipeline and asserts that every
// parse-derived fact in the manifest is actually present in the index — and,
// critically, that every NodeKey resolves byte-for-byte to the node_id the
// pipeline emits (which simultaneously validates ResolveID's path
// reconstruction and the frozen node facts).
//
// If you edit the fixture source under testdata/, this test tells you exactly
// which manifest facts went stale.

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func TestManifestNodesResolveToRealIDs(t *testing.T) {
	db := indexFixtures(t)
	actual := allNodeIDs(t, db)

	for _, repoID := range []string{AlphaRepoID, BetaRepoID} {
		root := testdataRoot(t, repoID)
		for _, nk := range Manifest().Nodes {
			if nodeRepo(nk) != repoID {
				continue
			}
			id := nk.ResolveID(repoID, root)
			if !actual[string(id)] {
				t.Errorf("node %+v: ResolveID=%s not present in index for repo %s", nk, id, repoID)
			}
		}
	}
}

func TestManifestEdgesPresent(t *testing.T) {
	db := indexFixtures(t)
	have := allEdges(t, db)

	for _, e := range Manifest().Edges {
		root := testdataRoot(t, e.RepoID)
		src := string(e.Src.ResolveID(e.RepoID, root))
		dst := string(e.Dst.ResolveID(e.RepoID, root))
		key := edgeRow{repo: e.RepoID, kind: string(e.Kind), src: src, dst: dst}
		if !have[key] {
			t.Errorf("edge %s %+v->%+v not present in index", e.Kind, e.Src, e.Dst)
		}
	}
}

func TestManifestCrossRepoEdgesPresent(t *testing.T) {
	db := indexFixtures(t)

	for _, ce := range Manifest().CrossRepoEdges {
		root := testdataRoot(t, ce.RepoID)
		src := string(ce.Src.ResolveID(ce.RepoID, root))
		var n int
		err := db.QueryRow(`
			SELECT COUNT(*) FROM cross_repo_edge_stubs
			WHERE repo_id=? AND branch=? AND src_node_id=? AND kind=? AND module_path=? AND symbol_path=?`,
			ce.RepoID, FixtureBranch, src, string(ce.Kind), ce.ModulePath, ce.Symbol,
		).Scan(&n)
		if err != nil {
			t.Fatalf("query stub: %v", err)
		}
		if n == 0 {
			t.Errorf("cross-repo stub %s/%s from %+v not present", ce.ModulePath, ce.Symbol, ce.Src)
		}
	}
}

func TestManifestDependenciesPresent(t *testing.T) {
	db := indexFixtures(t)

	for _, dep := range Manifest().Dependencies {
		var n int
		// file_imports.file_path is the absolute walked path; match on the
		// relative-path suffix so the assertion is machine-independent.
		err := db.QueryRow(`
			SELECT COUNT(*) FROM file_imports
			WHERE repo_id=? AND branch=? AND import_path=? AND file_path LIKE ?`,
			dep.RepoID, FixtureBranch, dep.ImportPath, "%"+dep.FromRelPath,
		).Scan(&n)
		if err != nil {
			t.Fatalf("query file_imports: %v", err)
		}
		if n == 0 {
			t.Errorf("dependency %s imports %q not present in file_imports", dep.FromRelPath, dep.ImportPath)
		}
	}
}

func TestManifestEntryPointsPresent(t *testing.T) {
	db := indexFixtures(t)
	actual := allNodeIDs(t, db)

	for _, ep := range Manifest().EntryPoints {
		if !isEntryPointKind(ep.Node.Kind) {
			t.Errorf("entry point %+v has kind %q that is not an entry-point kind", ep.Node, ep.Node.Kind)
		}
		root := testdataRoot(t, ep.RepoID)
		id := string(ep.Node.ResolveID(ep.RepoID, root))
		if !actual[id] {
			t.Errorf("entry point node %+v not present in index", ep.Node)
		}
	}
}

func TestManifestTodosPresent(t *testing.T) {
	db := indexFixtures(t)

	for _, td := range Manifest().Todos {
		var msg string
		// One rule='todo' finding per file; its message embeds the absolute
		// path and the "Lnn:" line marker. Assert on the relative-path suffix
		// and the line, not the full (machine-specific) message.
		err := db.QueryRow(`
			SELECT message FROM findings
			WHERE repo_id=? AND branch=? AND rule='todo' AND file_path LIKE ?`,
			td.RepoID, FixtureBranch, "%"+td.RelPath,
		).Scan(&msg)
		if err == sql.ErrNoRows {
			t.Errorf("no todo finding for %s/%s", td.RepoID, td.RelPath)
			continue
		}
		if err != nil {
			t.Fatalf("query todo finding: %v", err)
		}
		if !strings.Contains(msg, td.Marker) {
			t.Errorf("todo finding for %s missing marker %q: %q", td.RelPath, td.Marker, msg)
		}
		if !strings.Contains(msg, "L"+strconv.Itoa(td.Line)) {
			t.Errorf("todo finding for %s missing line L%d: %q", td.RelPath, td.Line, msg)
		}
	}
}

func TestManifestClonePairPresent(t *testing.T) {
	db := indexFixtures(t)
	actual := allNodeIDs(t, db)

	for _, c := range Manifest().Clones {
		root := testdataRoot(t, c.RepoID)
		for _, nk := range []NodeKey{c.A, c.B} {
			id := string(nk.ResolveID(c.RepoID, root))
			if !actual[id] {
				t.Errorf("clone member %+v not present in index", nk)
			}
		}
	}
}

// --- helpers ---

func nodeRepo(nk NodeKey) string {
	if strings.HasPrefix(nk.Path, "metric/") {
		return AlphaRepoID
	}
	return BetaRepoID
}

func testdataRoot(t *testing.T, repoID string) string {
	t.Helper()
	switch repoID {
	case AlphaRepoID:
		return testdataDir(t, "modalpha")
	case BetaRepoID:
		return testdataDir(t, "modbeta")
	default:
		t.Fatalf("unknown repoID %q", repoID)
		return ""
	}
}

func allNodeIDs(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT node_id FROM nodes`)
	if err != nil {
		t.Fatalf("query node_ids: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan node_id: %v", err)
		}
		out[id] = true
	}
	return out
}

type edgeRow struct{ repo, kind, src, dst string }

func allEdges(t *testing.T, db *sql.DB) map[edgeRow]bool {
	t.Helper()
	rows, err := db.Query(`SELECT repo_id, kind, src_node_id, dst_node_id FROM edges`)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer rows.Close()
	out := map[edgeRow]bool{}
	for rows.Next() {
		var r edgeRow
		if err := rows.Scan(&r.repo, &r.kind, &r.src, &r.dst); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		out[r] = true
	}
	return out
}

// isEntryPointKind mirrors wiki.isEntryPointKind (unexported there) so the
// self-test can sanity-check the EntryPointFact kinds without importing the
// wiki package's internals.
func isEntryPointKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindFunction, domain.KindMethod, domain.KindType,
		domain.KindStruct, domain.KindInterface, domain.KindClass,
		domain.KindCommand, domain.KindRoute:
		return true
	default:
		return false
	}
}
