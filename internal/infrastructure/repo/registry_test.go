package repo_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// createReposTable mirrors the schema after migration 0013 (kxo5.2) so
// tests touching canonical_url / kind / last_accessed_at / prompted_at can
// exercise the real column set.
const createReposTable = `
CREATE TABLE repos (
	repo_id          TEXT PRIMARY KEY,
	root_path        TEXT NOT NULL UNIQUE,
	added_at         INTEGER NOT NULL,
	active_branch    TEXT,
	last_promoted_sha TEXT,
	module_path      TEXT,
	kind             TEXT NOT NULL DEFAULT 'tracked',
	canonical_url    TEXT,
	last_accessed_at INTEGER,
	prompted_at      INTEGER
);
CREATE UNIQUE INDEX idx_repos_canonical_url
	ON repos(canonical_url)
	WHERE canonical_url IS NOT NULL;
CREATE TABLE repo_aliases (
	name     TEXT PRIMARY KEY,
	repo_id  TEXT NOT NULL,
	FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_repo_aliases_repo_id ON repo_aliases(repo_id);`

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if _, err := db.Exec(createReposTable); err != nil {
		t.Fatalf("create repos table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newGitRepo creates a temp directory with a .git/hooks/ subdirectory.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	return dir
}

func TestAddRepo(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if repoID == "" {
		t.Fatal("repoID is empty")
	}

	var rootPath string
	err = db.QueryRow("SELECT root_path FROM repos WHERE repo_id = ?", repoID).Scan(&rootPath)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}

	canonical, _ := filepath.EvalSymlinks(dir)
	if rootPath != canonical {
		t.Errorf("root_path = %q, want %q", rootPath, canonical)
	}
}

func TestAddRepoIdempotent(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	id1, existed1, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if existed1 {
		t.Errorf("first Add: existed=true, want false")
	}
	id2, existed2, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if !existed2 {
		t.Errorf("second Add: existed=false, want true ")
	}
	if id1 != id2 {
		t.Errorf("idempotent: id1=%s id2=%s differ", id1, id2)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM repos WHERE repo_id = ?", id1).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestAddRepoReadsGoMod(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	gomod := "module github.com/foo/bar\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var modPath sql.NullString
	if err := db.QueryRow("SELECT module_path FROM repos WHERE repo_id = ?", repoID).Scan(&modPath); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !modPath.Valid || modPath.String != "github.com/foo/bar" {
		t.Errorf("module_path = %v, want github.com/foo/bar", modPath)
	}
}

func TestAddRepoReadsPackageJSON(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	pkgJSON, _ := json.Marshal(map[string]string{"name": "@scope/pkg", "version": "1.0.0"})
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var modPath sql.NullString
	if err := db.QueryRow("SELECT module_path FROM repos WHERE repo_id = ?", repoID).Scan(&modPath); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !modPath.Valid || modPath.String != "@scope/pkg" {
		t.Errorf("module_path = %v, want @scope/pkg", modPath)
	}
}

func TestAddRepoInstallsHooks(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	if _, _, err := repo.Add(context.Background(), db, dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, hook := range []string{"post-commit", "post-checkout"} {
		hookPath := filepath.Join(dir, ".git", "hooks", hook)
		info, err := os.Stat(hookPath)
		if err != nil {
			t.Errorf("hook %s not found: %v", hook, err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("hook %s is not executable (mode %v)", hook, info.Mode())
		}
	}
}

// TestAddRepoDetectsActiveBranch covers solov2-f8p: a real `git init -b <name>`
// working tree records its branch in repos.active_branch. Without this every
// downstream write keys by branch="" and the graph becomes unqueryable.
func TestAddRepoDetectsActiveBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, branch := range []string{"main", "trunk", "feature/x"} {
		t.Run(branch, func(t *testing.T) {
			db := newTestDB(t)
			dir := t.TempDir()
			runGitTest(t, dir, "init", "-q", "-b", branch)
			// symbolic-ref needs an initial commit to anchor the branch
			// on some git versions; create a no-op commit.
			runGitTest(t, dir, "config", "user.email", "t@t")
			runGitTest(t, dir, "config", "user.name", "T")
			runGitTest(t, dir, "commit", "-q", "--allow-empty", "-m", "init")

			repoID, _, err := repo.Add(context.Background(), db, dir)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			var got sql.NullString
			if err := db.QueryRow(
				`SELECT active_branch FROM repos WHERE repo_id = ?`, repoID,
			).Scan(&got); err != nil {
				t.Fatalf("query active_branch: %v", err)
			}
			if got.String != branch {
				t.Errorf("active_branch = %q, want %q", got.String, branch)
			}
		})
	}
}

// TestAddRepoDefaultsBranchWhenDetectionFails covers the "freshly-init'd repo
// with no commits / not actually a git tree" path: detection fails, but Add
// must still produce a usable branch so the downstream pipeline is not
// silently broken. The existing newGitRepo helper creates only .git/hooks
// (no real git init), so this path is what every other test in this file
// exercises by construction.
func TestAddRepoDefaultsBranchWhenDetectionFails(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var got sql.NullString
	if err := db.QueryRow(
		`SELECT active_branch FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&got); err != nil {
		t.Fatalf("query active_branch: %v", err)
	}
	if got.String != "main" {
		t.Errorf("active_branch = %q, want %q (fallback)", got.String, "main")
	}
}

// TestAddRepoHookUsesAbsoluteBinaryPath covers solov2-v7q: installed hooks
// must invoke the absolute path of the veska CLI binary, not bare "veska"
// (broken for any non-PATH install) and NOT the daemon path either (which
// has no 'hook-runner' subcommand — exposed by the second journey pass).
func TestAddRepoHookUsesAbsoluteBinaryPath(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	if _, _, err := repo.Add(context.Background(), db, dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", "post-commit"))
	if err != nil {
		t.Fatalf("read post-commit: %v", err)
	}
	script := string(body)
	if strings.Contains(script, "exec veska hook-runner") {
		t.Errorf("post-commit invokes bare 'veska'; want absolute path. Body:\n%s", script)
	}
	if !strings.Contains(script, "exec /") {
		t.Errorf("post-commit does not invoke an absolute path. Body:\n%s", script)
	}
	// The hook must point at the CLI binary, not at veska-daemon /
	// veska-mcp — those have no 'hook-runner' subcommand. When the test
	// binary itself has neither suffix this is trivially true; the
	// daemon-suffix case is exercised by TestVeskaBinary_StripsDaemonSuffix
	// below.
	if strings.Contains(script, "veska-daemon hook-runner") ||
		strings.Contains(script, "veska-mcp hook-runner") {
		t.Errorf("post-commit invokes a non-CLI sibling. Body:\n%s", script)
	}
}

// TestVeskaBinary_StripsDaemonSuffix covers the daemon-side of v7q exposed
// during the second journey pass: when repo.Add runs inside veska-daemon,
// os.Executable returns ".../veska-daemon" — but the post-commit hook MUST
// invoke ".../veska hook-runner …" because veska-daemon has no hook-runner
// subcommand. We simulate this by symlinking the test binary as both
// "veska" (the sibling we want resolved) and "veska-daemon" (the running
// process) and asserting the hook script picks the CLI path.
func TestVeskaBinary_StripsDaemonSuffix(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "veska")
	daemonPath := filepath.Join(dir, "veska-daemon")
	// The two siblings must both exist for the resolver to pick the CLI.
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write cli stub: %v", err)
	}
	if err := os.WriteFile(daemonPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write daemon stub: %v", err)
	}
	got := repo.ResolveVeskaBinaryForTest(daemonPath)
	if got != cliPath {
		t.Errorf("ResolveVeskaBinaryForTest(%q) = %q, want %q", daemonPath, got, cliPath)
	}
}

// runGitTest shells `git -C dir <args>`, failing the test on non-zero exit.
func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestRemoveRepo(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := repo.Remove(context.Background(), db, repoID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM repos WHERE repo_id = ?", repoID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after Remove, got %d", count)
	}
}

// TestRemoveRepoDeletesEmbeddingRefs pins solov2-khra: node_embedding_refs
// has no FK to nodes (composite PK), so the repos CASCADE that clears a
// removed repo's nodes leaves its refs orphaned. Remove must delete them
// explicitly, or they linger as 'pending' rows that pin eng_get_status at
// degraded forever.
func TestRemoveRepoDeletesEmbeddingRefs(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)
	ctx := context.Background()

	repoID, _, err := repo.Add(ctx, db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// newTestDB only creates the repos tables; add the minimal nodes +
	// refs shape this path touches.
	if _, err := db.Exec(`
		CREATE TABLE nodes (node_id TEXT, branch TEXT, repo_id TEXT, language TEXT,
			kind TEXT, symbol_path TEXT, file_path TEXT, content_hash TEXT,
			last_promoted_at INTEGER, actor_id TEXT, actor_kind TEXT,
			PRIMARY KEY (node_id, branch));
		CREATE TABLE node_embedding_refs (node_id TEXT PRIMARY KEY, content_hash TEXT,
			state TEXT NOT NULL, enqueued_at INTEGER NOT NULL, embedded_at INTEGER,
			attempts INTEGER NOT NULL DEFAULT 0);`); err != nil {
		t.Fatalf("create nodes/refs tables: %v", err)
	}

	// Seed a node for the repo plus a pending ref into it.
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n-khra", "main", repoID, "go", "function", "n-khra", "f.go",
		"h", now, "test", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
		"n-khra", now); err != nil {
		t.Fatalf("insert ref: %v", err)
	}

	if err := repo.Remove(ctx, db, repoID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var refs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE node_id='n-khra'`).
		Scan(&refs); err != nil {
		t.Fatalf("count refs: %v", err)
	}
	if refs != 0 {
		t.Errorf("expected embedding refs deleted on repo removal, got %d", refs)
	}
}

// TestRemoveRepoByShortPrefix pins solov2-d78r: Remove must accept a unique
// short id prefix (as printed by `veska repo add`). Before the fix the
// exact-match DELETE silently no-op'd on a short id, leaving the repo (and its
// CASCADE-able children) in place.
func TestRemoveRepoByShortPrefix(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	short := repoID[:12]
	if err := repo.Remove(context.Background(), db, short); err != nil {
		t.Fatalf("Remove by short prefix: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM repos WHERE repo_id = ?", repoID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected repo removed via short prefix, got %d rows", count)
	}

	// An unknown id is now a loud error, not a silent success.
	if err := repo.Remove(context.Background(), db, "ffffffffffff"); err == nil {
		t.Error("expected error removing unknown repo id, got nil")
	}
}

func TestRemoveRepoRemovesHooks(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Confirm hooks are present first.
	for _, hook := range []string{"post-commit", "post-checkout"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", hook)); err != nil {
			t.Fatalf("hook %s missing after Add: %v", hook, err)
		}
	}

	if err := repo.Remove(context.Background(), db, repoID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, hook := range []string{"post-commit", "post-checkout"} {
		hookPath := filepath.Join(dir, ".git", "hooks", hook)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("hook %s still exists after Remove (err=%v)", hook, err)
		}
	}
}

func TestList_ReturnsRegisteredRepos(t *testing.T) {
	db := newTestDB(t)

	// One fully-populated row.
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, last_promoted_sha, module_path)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"id-b", "/path/b", 100, "main", "abc123", "mod/b",
	); err != nil {
		t.Fatalf("insert row b: %v", err)
	}
	// One row with NULL active_branch + last_promoted_sha.
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path)
		 VALUES (?, ?, ?, ?)`,
		"id-a", "/path/a", 50, "mod/a",
	); err != nil {
		t.Fatalf("insert row a: %v", err)
	}

	got, err := repo.List(context.Background(), db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List) = %d, want 2", len(got))
	}

	// ORDER BY repo_id: id-a then id-b.
	if !reflect.DeepEqual(got[0], repo.Record{RepoID: "id-a", RootPath: "/path/a", Kind: "tracked"}) {
		t.Errorf("got[0] = %+v, want id-a with empty nullable fields", got[0])
	}
	if !reflect.DeepEqual(got[1], repo.Record{
		RepoID: "id-b", RootPath: "/path/b", ActiveBranch: "main", LastPromotedSHA: "abc123", Kind: "tracked",
	}) {
		t.Errorf("got[1] = %+v, want fully-populated id-b", got[1])
	}
}

func TestList_EmptyTable(t *testing.T) {
	db := newTestDB(t)

	got, err := repo.List(context.Background(), db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got != nil {
		t.Errorf("List on empty table = %+v, want nil slice", got)
	}
}

func TestGet_ReturnsRegisteredRepo(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, last_promoted_sha, module_path)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"id-x", "/path/x", 7, "main", "sha-x", "mod/x",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.Get(context.Background(), db, "id-x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := repo.Record{
		RepoID: "id-x", RootPath: "/path/x", ActiveBranch: "main", LastPromotedSHA: "sha-x",
		Kind: "tracked", // migration 0013 default
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
}

func TestGet_NullableColumnsFlattened(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"id-n", "/path/n", 1,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.Get(context.Background(), db, "id-n")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveBranch != "" || got.LastPromotedSHA != "" {
		t.Errorf("Get nullable fields not flattened: %+v", got)
	}
}

func TestGet_MissingRowReturnsZero(t *testing.T) {
	db := newTestDB(t)
	got, err := repo.Get(context.Background(), db, "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, repo.Record{}) {
		t.Errorf("Get missing = %+v, want zero Record", got)
	}
}

func TestDerivedRepoIDFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		canonical string
	}{
		{"github https", "https://github.com/foo/bar"},
		{"gitlab https", "https://gitlab.com/group/proj"},
		{"self-hosted https port", "https://git.example.com:8443/team/repo"},
		{"ssh-normalised form", "https://github.com/foo/bar"},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := repo.DerivedRepoIDFromURL(tc.canonical)
			if len(id) != 64 {
				t.Errorf("id length: want 64 hex chars, got %d (%q)", len(id), id)
			}
			for _, c := range id {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("id contains non-hex char %q in %q", c, id)
					break
				}
			}
			if again := repo.DerivedRepoIDFromURL(tc.canonical); again != id {
				t.Errorf("not deterministic: %q vs %q", id, again)
			}
			if prev, ok := seen[tc.canonical]; ok && prev != id {
				t.Errorf("same canonical url produced different ids: %q vs %q", prev, id)
			}
			seen[tc.canonical] = id
		})
	}

	// Different canonical URLs must produce different ids.
	a := repo.DerivedRepoIDFromURL("https://github.com/foo/bar")
	b := repo.DerivedRepoIDFromURL("https://github.com/foo/baz")
	if a == b {
		t.Error("distinct urls produced the same id")
	}
}
