package diffgatecmd

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

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
// as a NEW finding even though lib.go was not in the change set — which only
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
	// cross-file call — so a new finding here means the call is GENUINELY gone,
	// not that resolution silently broke.
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand-sha",
		map[string][]byte{
			"lib.go":  []byte("package p\n\nfunc helper() {}\n"),
			"main.go": []byte("package p\n\nfunc Run() {}\n"),
		},
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
		map[string][]byte{
			"lib.go":  []byte("package p\n\nfunc helper() {}\n"),
			"main.go": []byte("package p\n\n// run it\nfunc Run() { helper() }\n"),
		},
	)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}
	if newIDs := setDiff(disc.CandidateIDs, disc.BaseIDs); len(newIDs) != 0 {
		t.Fatalf("expected no new findings for a no-impact change; got %v", newIDs)
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
