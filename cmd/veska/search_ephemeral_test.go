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

	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/repo"
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
	rec, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w)
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
	rec1, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w1)
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
	rec2, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &w2)
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

	rec1, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Simulate a user wiping ~/.cache.
	if err := os.RemoveAll(rec1.RootPath); err != nil {
		t.Fatal(err)
	}

	rec2, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
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

	rec, err := ephemeralEnsureFromURL(context.Background(), pools, "file://"+source, &bytes.Buffer{})
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
	if isGitURL(tmp) {
		t.Errorf("isGitURL(%q) = true, want false (existing path)", tmp)
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
