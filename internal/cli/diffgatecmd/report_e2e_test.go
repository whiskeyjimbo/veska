package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

type prReport struct {
	ChangedFiles []string `json:"changed_files"`
	BlastRadius  struct {
		SeedFiles int `json:"seed_files"`
		NodeCount int `json:"node_count"`
		Entries   []struct {
			SymbolPath string `json:"symbol_path"`
		} `json:"entries"`
	} `json:"blast_radius"`
	ChangeRisk []struct {
		FilePath string `json:"file_path"`
		Score    int    `json:"score"`
	} `json:"change_risk"`
	OpenFindings []struct {
		Rule     string `json:"rule"`
		FilePath string `json:"file_path"`
	} `json:"open_findings"`
	Untested []struct {
		NodeID string `json:"node_id"`
	} `json:"untested_changed"`
	Notes []string `json:"notes"`
}

func runReport(t *testing.T, home, repoDir string) (prReport, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunReport(context.Background(), ReportParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	var r prReport
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatalf("report JSON: %v\nraw: %s", jerr, out.String())
	}
	return r, err
}

func notesContain(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// LOAD-BEARING (AC2/DoD): the report ALWAYS exits 0, even degraded. An un-indexed
// repo — the most common early-adoption state — must yield a noted report with a
// nil error (the gates fail closed here; the advisory report must not). This is
// the divergence that makes "drop it in CI, it never breaks the build" true.
func TestRunReport_E2E_NotIndexed_ExitsZeroWithNote(t *testing.T) {
	home := t.TempDir()
	// Registered repo, but nothing promoted -> repoIndexed == false.
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{})

	r, err := runReport(t, home, t.TempDir())
	if err != nil {
		t.Fatalf("advisory report must exit 0 when not indexed; got err=%v", err)
	}
	if !notesContain(r.Notes, "repo_not_indexed") {
		t.Fatalf("want a repo_not_indexed note; got %+v", r.Notes)
	}
}

// DoD: each impact section is populated from a fixture diff — and the report
// exits 0 (nil err) even though it carries content (untested symbols present),
// proving content never gates. Base: a.go A->B, b.go B. Candidate modifies b.go
// (so it has base nodes -> a real blast seed) and adds an untested prod symbol.
func TestRunReport_E2E_PopulatesSections(t *testing.T) {
	home := t.TempDir()
	const aSrc = "package p\n\nfunc A() int { return B() }\n"
	const bSrc = "package p\n\nfunc B() int { return 1 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"a.go": aSrc, "b.go": bSrc})
	repoDir := t.TempDir()
	candB := "package p\n\nfunc B() int { return 2 }\n\nfunc New() int { return 0 }\n" // modify B + add untested New
	makeRepo(t, repoDir,
		map[string]string{"a.go": aSrc, "b.go": bSrc},
		map[string]*string{"b.go": &candB}, // a.go unchanged
	)

	r, err := runReport(t, home, repoDir)
	if err != nil {
		t.Fatalf("advisory report must exit 0 with content present; got err=%v", err)
	}
	if len(r.ChangedFiles) == 0 {
		t.Fatalf("changed_files empty — path/diff wiring broken; got %+v", r)
	}
	if r.BlastRadius.NodeCount == 0 || len(r.BlastRadius.Entries) == 0 {
		t.Fatalf("blast_radius section empty; got %+v", r.BlastRadius)
	}
	if len(r.ChangeRisk) == 0 || r.ChangeRisk[0].Score == 0 {
		t.Fatalf("change_risk section empty or zero-score (fixture needs churn × blast); got %+v", r.ChangeRisk)
	}
	if len(r.Untested) == 0 {
		t.Fatalf("untested_changed section empty; got %+v", r.Untested)
	}
}

// DoD: open findings on touched files are surfaced. Seeds a file-anchored open
// finding on b.go, then changes b.go — the report's open_findings must list it,
// and still exit 0.
func TestRunReport_E2E_OpenFindings_Populated(t *testing.T) {
	home := t.TempDir()
	const bSrc = "package p\n\nfunc B() int { return 1 }\n"
	dbPath := filepath.Join(home, "veska.db")
	seedBaseDB(t, dbPath, map[string]string{"b.go": bSrc})
	seedFileFinding(t, dbPath, discRepo, discBranch, "b.go", "dead-code")

	repoDir := t.TempDir()
	candB := "package p\n\nfunc B() int { return 2 }\n"
	makeRepo(t, repoDir,
		map[string]string{"b.go": bSrc},
		map[string]*string{"b.go": &candB},
	)

	r, err := runReport(t, home, repoDir)
	if err != nil {
		t.Fatalf("advisory report must exit 0 with findings present; got err=%v", err)
	}
	if len(r.OpenFindings) == 0 || r.OpenFindings[0].FilePath != "b.go" {
		t.Fatalf("open_findings must list the b.go finding; got %+v", r.OpenFindings)
	}
}

// seedFileFinding inserts one open, file-anchored finding for the test fixtures.
func seedFileFinding(t *testing.T, dbPath, repoID, branch, filePath, rule string) {
	t.Helper()
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools for finding seed: %v", err)
	}
	defer pools.Close()
	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID: repoID, Branch: branch,
		Severity: domain.SeverityMedium, Layer: domain.LayerStructural,
		Rule: rule, Message: "seeded fixture finding",
	}, domain.WithFileAnchor(filePath))
	if err != nil {
		t.Fatalf("build finding: %v", err)
	}
	if err := sqlite.NewFindingRepo(pools.Write).Save(context.Background(), f); err != nil {
		t.Fatalf("save finding: %v", err)
	}
}
