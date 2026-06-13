package diffgatecmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

func openReadDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, sqldriver.BuildDSN(dbPath, 5000))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeRepo creates a git repo at dir with a base commit (baseFiles) and a
// candidate commit (the result of applying candFiles: a nil value deletes).
// Returns when HEAD~1 is base and HEAD is candidate.
func makeRepo(t *testing.T, dir string, baseFiles map[string]string, candFiles map[string]*string) {
	t.Helper()
	runGit(t, dir, "init", "-q", "-b", "main")
	for path, src := range baseFiles {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base")
	for path, src := range candFiles {
		full := filepath.Join(dir, path)
		if src == nil {
			_ = os.Remove(full)
			continue
		}
		if err := os.WriteFile(full, []byte(*src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "candidate")
}

// nodeIDByName returns the node_id of the named symbol in the seeded base db.
func nodeIDByName(t *testing.T, dbPath, name string) string {
	t.Helper()
	db := openReadDB(t, dbPath)
	defer db.Close()
	var id string
	if err := db.QueryRow(
		`SELECT node_id FROM nodes WHERE repo_id=? AND branch=? AND symbol_path LIKE '%' || ? || '%'`,
		discRepo, discBranch, name,
	).Scan(&id); err != nil {
		t.Fatalf("lookup node %q: %v", name, err)
	}
	return id
}

// runGate drives the full wired Run against the seeded base db (via VESKA_HOME)
// and the git repo, returning the parsed verdict and the Run error.
func runGate(t *testing.T, home, repoRoot, anchorID string) (diffgate.GateVerdict, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := Run(context.Background(), Params{
		RepoID:       discRepo,
		Branch:       discBranch,
		RepoRoot:     repoRoot,
		BaseRef:      "HEAD~1",
		CandidateRef: "HEAD",
		AnchorNodeID: anchorID,
		Rule:         "dead-code",
		Out:          &out,
	})
	var v diffgate.GateVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	return v, err
}

// TestRun_E2E_FailOnNewFinding: a change that removes the only caller of a
// cross-file helper makes it newly dead — the live gate, with discovery wired,
// must FAIL naming new_findings (and exit non-zero).
func TestRun_E2E_FailOnNewFinding(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	base := map[string]string{
		"lib.go":  "package p\n\nfunc helper() {}\n",
		"main.go": "package p\n\nfunc Run() { helper() }\n",
	}
	seedBaseDB(t, dbPath, base)

	repoDir := t.TempDir()
	noCall := "package p\n\nfunc Run() {}\n"
	makeRepo(t, repoDir, base, map[string]*string{"main.go": &noCall})

	v, err := runGate(t, home, repoDir, nodeIDByName(t, dbPath, "helper"))
	if err == nil {
		t.Fatalf("expected non-zero (FAIL) exit; verdict=%+v", v)
	}
	if v.Pass {
		t.Fatalf("expected FAIL; got %+v", v)
	}
	if !v.Verify.NewFindingsChecked {
		t.Fatalf("discovery should have run (NewFindingsChecked); got %+v", v.Verify)
	}
	if !slices.Contains(v.Failures, diffgate.FailNewFindings) {
		t.Fatalf("expected new_findings in failures; got %v", v.Failures)
	}
}

// TestRun_E2E_PassOnDeadCodeFix: the canonical fix — add an (exported, entry)
// caller of a dead symbol — resolves the finding, adds no new findings, stays
// in scope. The live gate, discovery wired, must PASS (exit zero).
func TestRun_E2E_PassOnDeadCodeFix(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	base := map[string]string{
		"x.go": "package p\n\nfunc dead() {}\n",
	}
	seedBaseDB(t, dbPath, base)

	repoDir := t.TempDir()
	fixed := "package p\n\nfunc dead() {}\n\nfunc User() { dead() }\n"
	makeRepo(t, repoDir, base, map[string]*string{"x.go": &fixed})

	v, err := runGate(t, home, repoDir, nodeIDByName(t, dbPath, "dead"))
	if err != nil {
		t.Fatalf("expected PASS (nil error); got err=%v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("expected PASS; got failures=%v verify=%+v scope=%+v", v.Failures, v.Verify, v.Scope)
	}
	if !v.Verify.TargetResolved || !v.Verify.NewFindingsChecked {
		t.Fatalf("expected resolved + discovery-checked; got %+v", v.Verify)
	}
	// The verdict must disclose WHICH rules discovery covered, so a PASS is not
	// read as "no new findings of ANY kind" (secrets/vuln are not covered).
	if !slices.Contains(v.Verify.NewFindingsCoveredRules, "dead-code") {
		t.Fatalf("verdict should disclose covered rules; got %v", v.Verify.NewFindingsCoveredRules)
	}
}

// TestRun_E2E_FindingFlag proves the ll57.11 ergonomic front door: a junior
// passes only --finding <id> (no --anchor/--rule), and the gate derives the
// anchor + rule from the stored finding and produces the correct PASS.
func TestRun_E2E_FindingFlag(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "veska.db")
	base := map[string]string{"x.go": "package p\n\nfunc dead() {}\n"}
	seedBaseDB(t, dbPath, base)

	// Generate the real dead-code finding on `dead` and capture its id.
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	ids, err := fullCheckPass(context.Background(), pools, buildStructuralRunner(pools), discRepo, discBranch, "seed")
	pools.Close()
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("expected a dead-code finding to seed")
	}
	findingID := ids[0]

	repoDir := t.TempDir()
	fixed := "package p\n\nfunc dead() {}\n\nfunc User() { dead() }\n"
	makeRepo(t, repoDir, base, map[string]*string{"x.go": &fixed})

	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err = Run(context.Background(), Params{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD",
		FindingID: findingID, // no --anchor / --rule
		Out:       &out,
	})
	var v diffgate.GateVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	if err != nil || !v.Pass {
		t.Fatalf("expected PASS via --finding alone; err=%v verdict=%+v", err, v)
	}
	if v.Verify.Rule != "dead-code" || !v.Verify.TargetResolved {
		t.Fatalf("expected rule derived to dead-code + resolved; got %+v", v.Verify)
	}
}

// TestRun_E2E_RepoNotIndexed: pointed at a fresh/empty VESKA_HOME, the gate must
// emit a clean JSON verdict (repo_not_indexed) and exit non-zero — NOT crash
// with a raw "no such table" and empty stdout (the ll57.8 finding, ll57.12 fix).
func TestRun_E2E_RepoNotIndexed(t *testing.T) {
	home := t.TempDir()
	migrated, err := sqlite.OpenWithOptions(filepath.Join(home, "veska.db"), sqlite.Options{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = migrated.Close() // schema present, but zero nodes for the repo

	repoDir := t.TempDir()
	base := map[string]string{"x.go": "package p\n\nfunc dead() {}\n"}
	fixed := "package p\n\nfunc dead() {}\n\nfunc User() { dead() }\n"
	makeRepo(t, repoDir, base, map[string]*string{"x.go": &fixed})

	v, err := runGate(t, home, repoDir, "anything")
	if err == nil {
		t.Fatalf("expected non-zero exit for an unindexed repo")
	}
	if v.Pass || !slices.Contains(v.Failures, diffgate.FailRepoNotIndexed) {
		t.Fatalf("expected a clean repo_not_indexed verdict (JSON emitted); got %+v", v)
	}
}
