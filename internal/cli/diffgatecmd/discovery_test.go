// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// readBaseRef serves every file's BASE content, standing in for
// git.FileAtRef at the base ref. The symmetric discovery reads ALL files at the
// base ref for the base side, so this must answer for every path, not just the
// unchanged ones.
func readBaseRef(_ context.Context, path string) ([]byte, error) {
	switch path {
	case "lib.go":
		return []byte("package p\n\nfunc helper() {}\n"), nil
	case "main.go":
		return []byte("package p\n\nfunc Run() { helper() }\n"), nil
	}
	return nil, nil
}

const discRepo = "repo-disc"
const discBranch = "main"

// seedBaseDB migrates a fresh DB at dbPath and promotes the given files through
// the real cold-scan pipeline, returning once the base graph is committed.
func seedBaseDB(t *testing.T, dbPath string, files map[string]string) {
	t.Helper()
	migrated, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("migrate base db: %v", err)
	}
	_ = migrated.Close()

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open base pools: %v", err)
	}
	defer pools.Close()
	// Register the repo so the promoter accepts the batch (in production the
	// clone inherits this row; the fixture seeds a minimal one).
	if _, err := pools.Write.ExecContext(context.Background(),
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		discRepo, "/tmp/repo-disc", 0); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	core := composition.NewColdScanCore(pools, nil, nil)
	for path, src := range files {
		core.Ingester.SaveColdScan(context.Background(), discRepo, discBranch, path, []byte(src))
	}
	if err := core.Promoter.Promote(context.Background(), discRepo, discBranch, "base-sha", discoverActor); err != nil {
		t.Fatalf("promote base: %v", err)
	}
}

// TestDiscoverStructural_CrossFileNewlyDead is the soundness proof for ll57.4:
// a CHANGED file (main.go) removes the only caller of a helper that lives in an
// UNCHANGED file (lib.go). The helper becomes dead. Discovery must surface that
// as a NEW finding even though lib.go was not in the change set - which only
// holds because the candidate check pass runs over the WHOLE graph, not just
// the changed files. A per-file scope would miss it (false green).
func TestDiscoverStructural_CrossFileNewlyDead(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	seedBaseDB(t, dbPath, map[string]string{
		"lib.go":  "package p\n\nfunc helper() {}\n",
		"main.go": "package p\n\nfunc Run() { helper() }\n",
	})

	// Complete candidate file set: lib.go UNCHANGED, main.go changed to drop the
	// call. The full batch is what lets the promoter rebind the (now absent)
	// cross-file call - so a new finding here means the call is GENUINELY gone,
	// not that resolution silently broke.
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		[]diffgate.FileChange{{Path: "main.go", Content: []byte("package p\n\nfunc Run() {}\n")}},
		readBaseRef,
	)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}
	if !disc.Ran {
		t.Fatalf("discovery should report Ran=true")
	}

	// helper has an inbound edge in the base → not dead → no base structural
	// finding. (This anchors "for the right reason": if resolution were broken,
	// base would already flag helper and the companion no-change test would
	// fail.)
	if len(disc.BaseIDs) != 0 {
		t.Fatalf("base should have no structural findings (helper is called); got %v", disc.BaseIDs)
	}
	// In the candidate, helper lost its caller → dead → a NEW finding appears,
	// despite lib.go being unchanged.
	newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs)
	if len(newIDs) == 0 {
		t.Fatalf("expected a NEW dead-code finding for the cross-file newly-dead helper; candidate=%v base=%v", disc.CandidateIDs, disc.BaseIDs)
	}
}

// TestDiscoverStructural_NoChangeNoNewFindings: re-promoting a file with no
// structural impact yields no new findings (the PASS path).
func TestDiscoverStructural_NoChangeNoNewFindings(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	seedBaseDB(t, dbPath, map[string]string{
		"lib.go":  "package p\n\nfunc helper() {}\n",
		"main.go": "package p\n\nfunc Run() { helper() }\n",
	})

	// Complete candidate file set: main.go gets a comment-only edit that KEEPS
	// the call. helper stays called → no new dead-code. This is the control
	// that fails if whole-candidate re-promote ever stops resolving the
	// cross-file call (the partial-batch bug that motivated this design).
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		[]diffgate.FileChange{{Path: "main.go", Content: []byte("package p\n\n// run it\nfunc Run() { helper() }\n")}},
		readBaseRef,
	)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}
	if newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs); len(newIDs) != 0 {
		t.Fatalf("expected no new findings for a no-impact change; got %v", newIDs)
	}
}

// files-only soundness proof: the CHANGED file (target.go) holds
// a symbol whose only caller lives in an UNCHANGED file (caller.go). An inert
// (comment-only) edit must yield NO new finding - the inbound edge from the
// unchanged caller survives because re-promoting target.go re-mints `target`
// with the SAME node_id (sha256 of repo/file/kind/symbol), so caller.go's edge
// still resolves. A naive "re-promote drops the node" would false-positive here.
func TestDiscoverStructural_InertEditToCalledFileNoNewFindings(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	seedBaseDB(t, dbPath, map[string]string{
		"caller.go": "package p\n\nfunc Caller() { target() }\n",
		"target.go": "package p\n\nfunc target() {}\n",
	})
	readBase := func(_ context.Context, path string) ([]byte, error) {
		if path == "target.go" {
			return []byte("package p\n\nfunc target() {}\n"), nil
		}
		return nil, nil
	}

	// Only target.go changes (comment added); caller.go is untouched and NOT
	// re-promoted - its edge to target must keep target alive.
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		[]diffgate.FileChange{{Path: "target.go", Content: []byte("package p\n\n// keep\nfunc target() {}\n")}},
		readBase,
	)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}
	if newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs); len(newIDs) != 0 {
		t.Fatalf("inert edit to a called file must add no findings; got %v (base=%v cand=%v)", newIDs, disc.BaseIDs, disc.CandidateIDs)
	}
}

// TestDiscoverStructural_AddedFileSkippedAtBase proves the added-file edge case:
// a brand-new file (absent at the base ref) is promoted on the candidate side
// but SKIPPED on the base side (readBase reports ErrFileAbsentAtRef). A new file
// whose new function is unused is itself newly dead → a NEW finding, and the
// base side must not error on the absent file.
func TestDiscoverStructural_AddedFileSkippedAtBase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	seedBaseDB(t, dbPath, map[string]string{
		"main.go": "package p\n\nfunc Run() {}\n",
	})
	readBase := func(_ context.Context, path string) ([]byte, error) {
		// new.go did not exist at base → absence sentinel, must be skipped.
		return nil, fmt.Errorf("%w: %s", diffgate.ErrFileAbsentAtRef, path)
	}

	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		[]diffgate.FileChange{{Path: "new.go", Content: []byte("package p\n\nfunc orphan() {}\n")}},
		readBase,
	)
	if err != nil {
		t.Fatalf("DiscoverStructural (added file must not error at base): %v", err)
	}
	if newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs); len(newIDs) == 0 {
		t.Fatalf("an added file with an unused function should surface a new dead-code finding; base=%v cand=%v", disc.BaseIDs, disc.CandidateIDs)
	}
}

// TestDiscoverStructural_IndexAheadOfBaseStillDetectsNewFinding pins
// when the indexed graph sits AHEAD of base (e.g. a local
// post-commit hook already indexed HEAD), the clone inherits an open finding for
// the candidate's new dead symbol. Because dead-code is not an authoritative
// check, discovery's re-check never closes it, so a naive diff cancels it on
// both sides → false GREEN. After clearing the inherited findings, the genuinely
// new finding must surface. This seeds via a REAL check pass - the check-free
// seedBaseDB is exactly what hid this bug from the other fixtures.
func TestDiscoverStructural_IndexAheadOfBaseStillDetectsNewFinding(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	// Seed the graph in CANDIDATE state: greet is already dead (Run doesn't call it).
	seedBaseDB(t, dbPath, map[string]string{
		"app.go": "package p\n\nfunc Run() {}\n\nfunc greet() string { return \"hi\" }\n",
	})
	// Persist the dead-code finding the daemon would have left, via a real check
	// pass - the step seedBaseDB omits, which is why the other fixtures (clean
	// findings table) never reproduced the leak.
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	seeded, err := fullCheckPass(context.Background(), pools, buildStructuralRunner(pools), discRepo, discBranch, "seed")
	pools.Close()
	if err != nil {
		t.Fatalf("seed check pass: %v", err)
	}
	if len(seeded) == 0 {
		t.Fatalf("expected a seeded dead-code finding for greet")
	}

	// base content CALLS greet (alive at base); candidate ORPHANS it (newly dead).
	readBase := func(_ context.Context, _ string) ([]byte, error) {
		return []byte("package p\n\nfunc Run() { greet() }\n\nfunc greet() string { return \"hi\" }\n"), nil
	}
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		[]diffgate.FileChange{{Path: "app.go", Content: []byte("package p\n\nfunc Run() {}\n\nfunc greet() string { return \"hi\" }\n")}},
		readBase,
	)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}
	if newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs); len(newIDs) == 0 {
		t.Fatalf("greet is newly dead (base calls it, candidate orphans it) but discovery found no new finding - false GREEN (base=%v cand=%v)", disc.BaseIDs, disc.CandidateIDs)
	}
}

func setDiff(a, b []string) []string {
	bs := make(map[string]struct{}, len(b))
	for _, x := range b {
		bs[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bs[x]; !ok {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}
