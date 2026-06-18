package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// cloneChangeSource is a ChangeSource returning a fixed candidate file set,
// standing in for the git RefChangeSource so the e2e can drive the REAL parser
// without a git worktree.
type cloneChangeSource struct {
	changes []diffgate.FileChange
}

func (s cloneChangeSource) Changes(context.Context) ([]diffgate.FileChange, error) {
	return s.changes, nil
}

// TestCloneGate_RealParserDetectsCrossFileClone is the non-substitutable test:
// it drives the REAL treesitter parser on both sides and the REAL sqlite base.
// The base is seeded through the actual promotion pipeline (so a.go's Foo node
// carries the parser's true content_hash); the candidate adds b.go with a
// byte-identical Foo, parsed by the same Indexer. If the parser emitted empty or
// path-dependent hashes for function nodes, the hashes would not match and the
// gate would (wrongly) PASS - exactly the silent false-PASS the unit tests with
// synthetic hashes cannot catch. The gate must FAIL, naming the clone group.
func TestCloneGate_RealParserDetectsCrossFileClone(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	const fooBody = "package p\n\nfunc Foo() int {\n\tx := 1\n\ty := 2\n\treturn x + y\n}\n"
	seedBaseDB(t, dbPath, map[string]string{"a.go": fooBody})

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	defer pools.Close()

	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(pools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(pools.ReadDB),
	}

	ix, err := diffgate.NewIndexer(treesitter.NewGoParser())
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	// Candidate adds b.go with a byte-identical Foo - a new exact clone.
	src := cloneChangeSource{changes: []diffgate.FileChange{
		{Path: "b.go", Content: []byte(fooBody)},
	}}
	eph, err := ix.Index(context.Background(), discRepo, discBranch, base, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	v, err := diffgate.NewCloneGate().Evaluate(context.Background(), eph)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || !v.Checked {
		t.Fatalf("real-parser cross-file clone must FAIL (checked); got pass=%v checked=%v - likely empty/mismatched parser content_hash", v.Pass, v.Checked)
	}
	if len(v.NewClones) != 1 {
		t.Fatalf("want exactly one new clone group; got %d: %+v", len(v.NewClones), v.NewClones)
	}
	if got := len(v.NewClones[0].Members); got != 2 {
		t.Fatalf("want clone group of 2 members (a.go + b.go); got %d: %+v", got, v.NewClones[0].Members)
	}
}

// TestRunClones_E2E_FailsAndExitsNonZero drives the WIRED command path:
// RunClones -> git RefChangeSource -> CloneGate -> JSON report + ErrGateFailed.
// It proves the DoD's "non-zero exit on FAIL" end to end (a non-nil error is
// what the cobra layer turns into a non-zero process exit), not just at the
// verdict level.
func TestRunClones_E2E_FailsAndExitsNonZero(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	const foo = "package p\n\nfunc Foo() int {\n\tx := 1\n\treturn x\n}\n"
	seedBaseDB(t, dbPath, map[string]string{"a.go": foo})

	// git repo: base has a.go; candidate adds an identical Foo in b.go.
	repoDir := t.TempDir()
	dup := foo
	makeRepo(t, repoDir, map[string]string{"a.go": foo}, map[string]*string{"b.go": &dup})

	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunClones(context.Background(), CloneParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("expected ErrGateFailed (non-zero exit); got %v", err)
	}
	var rep struct {
		Pass      bool     `json:"pass"`
		Checked   bool     `json:"checked"`
		Failures  []string `json:"failures"`
		NewClones []struct {
			ContentHash string `json:"content_hash"`
		} `json:"new_clones"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	if rep.Pass || !rep.Checked {
		t.Fatalf("expected FAIL+checked verdict; got %+v", rep)
	}
	if len(rep.NewClones) != 1 || len(rep.Failures) != 1 || rep.Failures[0] != "new_clone" {
		t.Fatalf("expected one new_clone failure; got failures=%v clones=%+v", rep.Failures, rep.NewClones)
	}
}

// TestRunClones_E2E_IndexAhead_NowDetected is the lock: the index
// is seeded AHEAD (at the candidate's content - it already holds the cloned b.go),
// base-ref has only a.go. Before the base graph was pinned to base-ref, the live
// index already showed the clone (baseCount>=2) so the net-new group cancelled →
// false-PASS. With buildPinnedEphemeral the base clone re-promotes base-ref's
// changed files - and DELETES the added b.go the drifted index carried - so the
// base reflects base-ref (a.go only) and the gate correctly FAILs. This exercises
// the added-file delete path that a plain skip would miss.
func TestRunClones_E2E_IndexAhead_NowDetected(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	const foo = "package p\n\nfunc Foo() int {\n\tx := 1\n\treturn x\n}\n"
	// Index seeded AHEAD: BOTH a.go and the cloned b.go already promoted.
	seedBaseDB(t, dbPath, map[string]string{"a.go": foo, "b.go": foo})

	repoDir := t.TempDir()
	dup := foo
	// base-ref has only a.go; candidate ADDS the identical b.go.
	makeRepo(t, repoDir, map[string]string{"a.go": foo}, map[string]*string{"b.go": &dup})

	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunClones(context.Background(), CloneParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("index-ahead clone must now FAIL (zvh6.11); got %v\nraw: %s", err, out.String())
	}
}

// TestCloneGate_RealParserCleanModificationPasses is the PASS companion: a
// candidate whose Foo body differs from the base produces a unique hash, so no
// new clone group forms.
func TestCloneGate_RealParserCleanModificationPasses(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	seedBaseDB(t, dbPath, map[string]string{
		"a.go": "package p\n\nfunc Foo() int {\n\treturn 1\n}\n",
	})
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	defer pools.Close()
	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(pools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(pools.ReadDB),
	}
	ix, _ := diffgate.NewIndexer(treesitter.NewGoParser())
	src := cloneChangeSource{changes: []diffgate.FileChange{
		// Different body -> different content_hash -> no clone.
		{Path: "b.go", Content: []byte("package p\n\nfunc Bar() int {\n\treturn 99\n}\n")},
	}}
	eph, _ := ix.Index(context.Background(), discRepo, discBranch, base, src)
	v, err := diffgate.NewCloneGate().Evaluate(context.Background(), eph)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass {
		t.Fatalf("distinct new code must PASS; got %+v", v.NewClones)
	}
}
