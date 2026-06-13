package diffgatecmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestMeasure_DiscoveryCost measures the whole-repo discovery cost on a
// realistically-sized graph (this repo's own Go files) to decide whether the
// ll57.7 package-closure optimization is warranted. Env-guarded so it never
// runs in normal CI.
//
//	DIFFGATE_PERF=1 go test -tags sqlite_fts5 -run TestMeasure_DiscoveryCost -v ./internal/cli/diffgatecmd/
func TestMeasure_DiscoveryCost(t *testing.T) {
	if os.Getenv("DIFFGATE_PERF") == "" {
		t.Skip("set DIFFGATE_PERF=1 to run the discovery cost measurement")
	}
	repoRoot := "../../.." // package cwd is internal/cli/diffgatecmd

	// Collect every Go file under internal/ and cmd/ as the base file set.
	var rels []string
	for _, sub := range []string{"internal", "cmd"} {
		root := filepath.Join(repoRoot, sub)
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil
			}
			rel, _ := filepath.Rel(repoRoot, p)
			rels = append(rels, rel)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	migrated, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = migrated.Close()

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	if _, err := pools.Write.ExecContext(context.Background(),
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, discRepo, "/tmp/perf", 0); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Seed the base graph: promote every file.
	core := composition.NewColdScanCore(pools, nil, nil)
	seedStart := time.Now()
	for _, rel := range rels {
		content, rerr := os.ReadFile(filepath.Join(repoRoot, rel))
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		core.Ingester.SaveColdScan(context.Background(), discRepo, discBranch, rel, content)
	}
	if err := core.Promoter.Promote(context.Background(), discRepo, discBranch, "base", discoverActor); err != nil {
		t.Fatalf("promote base: %v", err)
	}
	seedDur := time.Since(seedStart)
	pools.Close()

	fi, _ := os.Stat(dbPath)

	// Candidate change: one file gets a trivial appended comment. Everything
	// else is read unchanged from the working tree.
	changedRel := rels[len(rels)/2]
	orig, _ := os.ReadFile(filepath.Join(repoRoot, changedRel))
	changed := append(append([]byte(nil), orig...), []byte("\n// diff-gate perf probe\n")...)
	readUnchanged := func(_ context.Context, path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoRoot, path))
	}

	discStart := time.Now()
	disc, err := DiscoverStructural(context.Background(), dbPath, discRepo, discBranch, "cand",
		[]diffgate.FileChange{{Path: changedRel, Content: changed}}, readUnchanged)
	discDur := time.Since(discStart)
	if err != nil {
		t.Fatalf("DiscoverStructural: %v", err)
	}

	t.Logf("MEASUREMENT: files=%d dbSize=%.1fMB seed(build base)=%v DISCOVERY(per gate run)=%v baseFindings=%d candFindings=%d changedFile=%s",
		len(rels), float64(fi.Size())/(1<<20), seedDur.Round(time.Millisecond), discDur.Round(time.Millisecond),
		len(disc.BaseIDs), len(disc.CandidateIDs), changedRel)
}
