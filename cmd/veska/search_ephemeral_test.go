package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/cli/searchcmd"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

func startsWith(s, prefix string) bool { return strings.HasPrefix(s, prefix) }

// makeBareSource creates a real git repo with one empty commit and returns
// its absolute path — suitable as a `file://` source for clone tests. Skips
// the test if git is unavailable so the suite still passes in a no-git
// environment.
func makeBareSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "user.email", "test@example.invalid"},
		{"-C", dir, "config", "user.name", "test"},
		{"-C", dir, "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func openPoolsAt(t *testing.T, dir string) *sqlite.Pools {
	t.Helper()
	dbPath := filepath.Join(dir, "veska.db")
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func TestEphemeralEnsureFromURL_CloneAndRegister(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeBareSource(t)
	pools := openPoolsAt(t, home)

	var w bytes.Buffer
	rec, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w)
	if err != nil {
		t.Fatalf("ephemeralEnsureFromURL: %v", err)
	}
	if rec.RepoID == "" {
		t.Fatal("empty repo_id")
	}
	if rec.Kind != "ephemeral" {
		t.Errorf("kind = %q, want ephemeral", rec.Kind)
	}
	// Cache dir is under VESKA_CACHE_HOME/repos/<url-hash>.
	wantPrefix := filepath.Join(home, "cache", "repos") + string(filepath.Separator)
	if rec.RootPath == wantPrefix || rec.RootPath == "" || !startsWith(rec.RootPath, wantPrefix) {
		t.Errorf("RootPath %q not under cache tier %q", rec.RootPath, wantPrefix)
	}

	// canonical_url stamped to the file:// URL.
	var canonical sql.NullString
	if err := pools.ReadDB.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, rec.RepoID).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if !canonical.Valid || canonical.String != "file://"+source {
		t.Errorf("canonical_url = %v, want file://%s", canonical, source)
	}
}

func TestEphemeralEnsureFromURL_CacheHitSkipsClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeBareSource(t)
	pools := openPoolsAt(t, home)

	var w1 bytes.Buffer
	rec1, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w1)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Mtime probe so we can prove no re-clone happened.
	info, err := os.Stat(filepath.Join(rec1.RootPath, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	firstMtime := info.ModTime()

	var w2 bytes.Buffer
	rec2, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if rec2.RepoID != rec1.RepoID {
		t.Errorf("cache hit returned different repo_id: %q vs %q", rec1.RepoID, rec2.RepoID)
	}
	info2, _ := os.Stat(filepath.Join(rec2.RootPath, ".git"))
	if !info2.ModTime().Equal(firstMtime) {
		t.Error("cache dir mtime changed — second call appears to have re-cloned")
	}

	// last_accessed_at was bumped on the cache hit (touch).
	var accessed sql.NullInt64
	if err := pools.ReadDB.QueryRow(`SELECT last_accessed_at FROM repos WHERE repo_id = ?`, rec2.RepoID).Scan(&accessed); err != nil {
		t.Fatal(err)
	}
	if !accessed.Valid || accessed.Int64 == 0 {
		t.Errorf("last_accessed_at = %v, want a positive unix timestamp", accessed)
	}
}

func TestEphemeralEnsureFromURL_MissingCacheDirReClones(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeBareSource(t)
	pools := openPoolsAt(t, home)

	rec1, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Simulate a user wiping ~/.cache.
	if err := os.RemoveAll(rec1.RootPath); err != nil {
		t.Fatal(err)
	}

	rec2, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("re-clone after wipe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rec2.RootPath, ".git")); err != nil {
		t.Errorf(".git not present after re-clone: %v", err)
	}
	// canonical_url stayed the same → DerivedRepoIDFromURL → same path,
	// new row though (the old one was deleted).
	if rec2.RootPath != rec1.RootPath {
		t.Errorf("re-clone landed at a different path: %q vs %q", rec1.RootPath, rec2.RootPath)
	}
}

func TestEphemeralEnsureFromURL_TrackedMatchSkipsClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeBareSource(t)
	pools := openPoolsAt(t, home)

	// Seed a tracked row whose canonical_url matches what the URL form
	// will canonicalise to. Mirrors the kxo5.4 origin-alias scenario.
	canonical, err := repo.CanonicalURL("file://" + source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, canonical_url, kind) VALUES (?, ?, ?, ?, 'tracked')`,
		"tracked-id", source, int64(1), canonical,
	); err != nil {
		t.Fatal(err)
	}

	rec, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ephemeralEnsureFromURL: %v", err)
	}
	if rec.RepoID != "tracked-id" {
		t.Errorf("returned repo_id %q, want tracked-id (no new clone)", rec.RepoID)
	}
	if rec.Kind != "tracked" {
		t.Errorf("kind = %q, want tracked", rec.Kind)
	}

	// No row was added to repos.
	var n int
	if err := pools.ReadDB.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (cache miss must not have created a new row)", n)
	}

	// And nothing was written into the cache tier.
	cacheRoot := filepath.Join(home, "cache", "repos")
	if entries, _ := os.ReadDir(cacheRoot); len(entries) != 0 {
		t.Errorf("cache tier got %d entries; tracked match must skip clone", len(entries))
	}
}

func TestIsGitURL_PreservesPositionalSemantics(t *testing.T) {
	// Existing test (search_test.go) covers true/false expectations for
	// the original 7 cases. Re-spot-check the new tracked-path-vs-URL
	// disambiguation that landed with kxo5.6.
	tmp := t.TempDir()
	if searchcmd.IsGitURL(tmp) {
		t.Errorf("searchcmd.IsGitURL(%q) = true, want false (existing path)", tmp)
	}
}

// makeSecretSource creates a real git repo with a Go file that hard-codes a
// synthetic AWS access-key — gitleaks's BuiltinScanner detects it and the
// docs allowlist does NOT cover this shape, so it survives
// to the findings table. Returns the absolute path of the new repo.
func makeSecretSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "user.email", "test@example.invalid"},
		{"-C", dir, "config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	// Synthetic AWS access key shape; not the canonical docs-allowlist key
	// (mirrors secretsscanner_test.go's awsKey fixture).
	content := "package leak\n\nconst Key = \"AKIAZQ7XFAKE1234ABCD\"\n"
	if err := os.WriteFile(filepath.Join(dir, "leak.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write leak.go: %v", err)
	}
	for _, args := range [][]string{
		{"-C", dir, "add", "leak.go"},
		{"-C", dir, "commit", "-q", "-m", "leak"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// TestSearchEphemeral_FirstPromotionRunsSecretCheck pins:
// the in-process `veska search --repo <url>` path (and equivalently a
// daemon-less `veska reindex`) must register the same post-promotion
// check chain as `veska repo add --wait`. Before the fix the cold-scan
// Promoter was built without a CheckRunner, so secret-leak/vuln-scan only
// fired after a separate `veska reindex` while the daemon was up. This
// test runs the real defaultReparserFactory against a repo whose HEAD
// commits a synthetic AWS access key in a Go file and asserts a
// `secret_leak` finding lands in the findings table on the first promote.
func TestSearchEphemeral_FirstPromotionRunsSecretCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeSecretSource(t)
	pools := openPoolsAt(t, home)

	var buf bytes.Buffer
	rec, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &buf)
	if err != nil {
		t.Fatalf("ephemeralEnsureFromURL: %v", err)
	}
	if rec.Kind != "ephemeral" {
		t.Fatalf("kind = %q, want ephemeral", rec.Kind)
	}

	// Run the same cold-scan reparser that ensureIndexed builds. The
	// embedder drain is skipped — the check chain runs synchronously
	// inside Promoter.Promote, before drainEmbedderQueue.
	loader := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := defaultReparserFactory(pools, loader)
	if err != nil {
		t.Fatalf("reparserFactory: %v", err)
	}
	appRec := application.RepoRecord{
		RepoID:       rec.RepoID,
		RootPath:     rec.RootPath,
		ActiveBranch: rec.ActiveBranch,
	}
	if appRec.ActiveBranch == "" {
		appRec.ActiveBranch = "main"
	}
	if err := reparser(context.Background(), appRec); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	// Assertion: a secret_leak finding for the ephemeral repo's leak.go.
	var (
		n          int
		samplePath sql.NullString
	)
	if err := pools.ReadDB.QueryRow(
		`SELECT COUNT(*), MAX(file_path) FROM findings WHERE repo_id = ? AND rule = 'secret_leak'`,
		rec.RepoID,
	).Scan(&n, &samplePath); err != nil {
		t.Fatalf("query findings: %v", err)
	}
	if n == 0 {
		t.Fatalf("want >= 1 secret_leak finding after first promotion, got 0 (ephemeral first-promotion check chain regressed)")
	}
	if samplePath.Valid && !strings.Contains(samplePath.String, "leak.go") {
		t.Errorf("secret_leak file_path = %q, want path containing leak.go", samplePath.String)
	}
}

// TestEphemeralPromotionPrintsCheckSummary guards: after
// the first ephemeral cold scan completes, the search command emits a
// one-line per-rule summary so a user who ran `veska search <q> --repo
// <url>` sees the check chain ran (and what it found) before the search
// results. Without this signal the user only sees "search: indexing… /
// search: index ready" and has no idea promotion checks fired.
func TestEphemeralPromotionPrintsCheckSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	t.Setenv("VESKA_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", "")

	source := makeSecretSource(t)
	pools := openPoolsAt(t, home)

	var buf bytes.Buffer
	rec, err := searchcmd.EphemeralEnsureFromURL(context.Background(), pools, "file://"+source, &buf)
	if err != nil {
		t.Fatalf("ephemeralEnsureFromURL: %v", err)
	}

	loader := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := defaultReparserFactory(pools, loader)
	if err != nil {
		t.Fatalf("reparserFactory: %v", err)
	}
	appRec := application.RepoRecord{
		RepoID:       rec.RepoID,
		RootPath:     rec.RootPath,
		ActiveBranch: rec.ActiveBranch,
	}
	if appRec.ActiveBranch == "" {
		appRec.ActiveBranch = "main"
	}
	if err := reparser(context.Background(), appRec); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	buf.Reset()
	searchcmd.EmitColdScanSummary(context.Background(), pools.ReadDB, &buf, appRec.RepoID, appRec.ActiveBranch)
	got := buf.String()
	if !strings.Contains(got, "secret_leak") {
		t.Errorf("expected secret_leak summary line; got %q", got)
	}
	if !strings.Contains(got, "finding") {
		t.Errorf("expected 'finding(s)' wording in summary; got %q", got)
	}
}

// TestEmitColdScanSummary_NoFindingsStaysQuiet guards that the helper
// does not print anything when no findings were produced — a clean
// promotion should not pollute the search output with empty
// "0 finding(s)" lines.
func TestEmitColdScanSummary_NoFindingsStaysQuiet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	pools := openPoolsAt(t, home)

	var buf bytes.Buffer
	searchcmd.EmitColdScanSummary(context.Background(), pools.ReadDB, &buf, "repo-with-nothing", "main")
	if buf.Len() != 0 {
		t.Errorf("expected silent helper on empty findings; got %q", buf.String())
	}
}

// Sanity check that config.RepoCachePath is the path we end up cloning into.
// Locks the integration contract between kxo5.5 and kxo5.6.
func TestEphemeralCacheDirIsRepoCachePath(t *testing.T) {
	t.Setenv("VESKA_CACHE_HOME", "/synthetic/cache")
	t.Setenv("XDG_CACHE_HOME", "")
	canonical := "file:///some/repo"
	want := config.RepoCachePath(repo.DerivedRepoIDFromURL(canonical))
	if want != "/synthetic/cache/repos/"+repo.DerivedRepoIDFromURL(canonical) {
		t.Errorf("contract drift: want %q", want)
	}
}
