package daemon

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestReschemeSuppressions_AllScopes is the ADR-S0017 acceptance gate
// (solov2-dchd.5): every suppression scope is carried forward across the
// node_id/repo_id re-key, and anything non-deterministic (a finding whose id
// folded a WithFindingKey discriminator → ambiguous rule+anchor lookup) is
// REPORTED, not silently dropped or mis-remapped. A suppression already in the
// new scheme is left untouched (idempotency / post-migration safety).
func TestReschemeSuppressions_AllScopes(t *testing.T) {
	db := openReschemeDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	const (
		oldRepo = "oldrepo"
		newRepo = "newrepo"
		root    = "/old/root"
		oldNode = "OLDNODE1"
		newNode = "NEWNODE1"
	)

	// Post-migration repos row (id already re-keyed in the pre-rescan step).
	mustExec(t, db, `INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		newRepo, root, now)

	// Snapshot tables (created empty by migration 0019; seed them here).
	mustExec(t, db, `INSERT INTO _dchd_old_repos (repo_id, root_path) VALUES (?,?)`, oldRepo, root)
	mustExec(t, db, `INSERT INTO _dchd_old_nodes (node_id, repo_id, file_path, kind, symbol_path) VALUES (?,?,?,?,?)`,
		oldNode, oldRepo, "/old/root/pkg/a.go", "function", "Foo")

	// Post-rescan node: same (rel path, kind, symbol) → the join target.
	insertNode(t, db, newNode, newRepo, "pkg/a.go", "function", "Foo", now)

	// Findings: OLDF1 -> unique new finding NEWF1 (remappable);
	// OLDF2 -> two new findings sharing (rule, anchor) (keyed → reported).
	mustExec(t, db, `INSERT INTO _dchd_old_findings (finding_id, repo_id, branch, node_id, file_path, rule) VALUES (?,?,?,?,?,?)`,
		"OLDF1", oldRepo, "main", oldNode, nil, "deadcode")
	mustExec(t, db, `INSERT INTO _dchd_old_findings (finding_id, repo_id, branch, node_id, file_path, rule) VALUES (?,?,?,?,?,?)`,
		"OLDF2", oldRepo, "main", oldNode, nil, "review")
	insertFinding(t, db, "NEWF1", newRepo, newNode, "deadcode", now)
	insertFinding(t, db, "NEWF2a", newRepo, newNode, "review", now)
	insertFinding(t, db, "NEWF2b", newRepo, newNode, "review", now)

	// Suppressions across every scope, plus one already-current symbol.
	supp := func(id, scope, target string) {
		mustExec(t, db, `INSERT INTO suppressions (suppression_id, scope, target, reason, created_at, actor_id, actor_kind) VALUES (?,?,?,?,?,?,?)`,
			id, scope, target, "r", now, "tester", "human")
	}
	supp("s-symbol", "symbol", oldNode)
	supp("s-find-ok", "finding", "OLDF1")
	supp("s-find-keyed", "finding", "OLDF2")
	supp("s-file", "file", "/old/root/pkg/a.go")
	supp("s-symbol-current", "symbol", newNode) // already new — must be left alone

	rep, err := reschemeSuppressions(ctx, db, db, repoIDMap{oldRepo: newRepo})
	if err != nil {
		t.Fatalf("reschemeSuppressions: %v", err)
	}

	wantTarget := map[string]string{
		"s-symbol":         newNode,
		"s-find-ok":        "NEWF1",
		"s-find-keyed":     "OLDF2",    // unchanged: reported, not guessed
		"s-file":           "pkg/a.go", // relativised
		"s-symbol-current": newNode,    // untouched
	}
	for id, want := range wantTarget {
		var got string
		if err := db.QueryRow(`SELECT target FROM suppressions WHERE suppression_id=?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != want {
			t.Errorf("suppression %s target = %q, want %q", id, got, want)
		}
	}

	if rep.remapped != 3 {
		t.Errorf("remapped = %d, want 3 (symbol, finding-ok, file)", rep.remapped)
	}
	if len(rep.unremappable) != 1 {
		t.Fatalf("unremappable = %d, want 1 (the keyed finding); got %+v", len(rep.unremappable), rep.unremappable)
	}
	if u := rep.unremappable[0]; u.suppressionID != "s-find-keyed" || u.scope != "finding" {
		t.Errorf("unremappable = %+v, want s-find-keyed/finding", u)
	}
}

// openReschemeDB opens a fully-migrated DB (through 0019, which creates the
// empty _dchd_old_* snapshot tables this test seeds). Requires the sqlite_fts5
// build tag at runtime (the migrations create FTS5 virtual tables).
func openReschemeDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := t.TempDir() + "/veska.db"
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRekeyRepoID_PreservesColumnsAndMovesChildren pins the repo_id re-key
// (solov2-dchd.4): the INSERT-new/move-children/DELETE-old dance must preserve
// EVERY repos column (a dropped column silently loses state), repoint live
// children (tasks, repo_aliases, repo-scope suppressions) onto the new id, and
// leave no trace of the old id.
func TestRekeyRepoID_PreservesColumnsAndMovesChildren(t *testing.T) {
	db := openReschemeDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	const (
		oldID  = "oldid"
		newID  = "newid"
		tier   = "module-hostpath"
		anchor = "github.com/org/repo"
	)

	// Seed a repos row with every nullable/non-null column populated so a
	// dropped column in the re-key INSERT would show up as a lost value.
	mustExec(t, db, `INSERT INTO repos
		(repo_id, root_path, added_at, active_branch, last_promoted_sha,
		 module_path, kind, canonical_url, last_accessed_at, prompted_at,
		 identity_tier, identity_anchor)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		oldID, "/root", now, "main", "deadbeef",
		"github.com/org/repo", "tracked", "https://github.com/org/repo", now, now,
		"abs-root", "/root")

	mustExec(t, db, `INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?,?,?,1,?)`, "t1", oldID, "task one", now)
	mustExec(t, db, `INSERT INTO repo_aliases (name, repo_id) VALUES (?,?)`, "myrepo", oldID)
	mustExec(t, db, `INSERT INTO suppressions (suppression_id, scope, target, reason, created_at, actor_id, actor_kind) VALUES (?,?,?,?,?,?,?)`,
		"sr", "repo", oldID, "r", now, "tester", "human")

	if err := rekeyRepoID(ctx, db, oldID, newID, tier, anchor); err != nil {
		t.Fatalf("rekeyRepoID: %v", err)
	}

	// Old id gone everywhere; new id present.
	var oldRepos int
	mustScan(t, db, `SELECT count(*) FROM repos WHERE repo_id=?`, &oldRepos, oldID)
	if oldRepos != 0 {
		t.Errorf("old repos row still present (%d)", oldRepos)
	}

	// All columns preserved on the new row, with only id/tier/anchor changed.
	var rootPath, branch, sha, modulePath, kind, canonicalURL, gotTier, gotAnchor string
	var addedAt, lastAccessed, promptedAt int64
	mustScanRow(t, db, `SELECT root_path, added_at, active_branch, last_promoted_sha,
		module_path, kind, canonical_url, last_accessed_at, prompted_at, identity_tier, identity_anchor
		FROM repos WHERE repo_id=?`, newID,
		&rootPath, &addedAt, &branch, &sha, &modulePath, &kind, &canonicalURL, &lastAccessed, &promptedAt, &gotTier, &gotAnchor)
	checks := map[string]struct{ got, want string }{
		"root_path":       {rootPath, "/root"},
		"active_branch":   {branch, "main"},
		"last_promoted":   {sha, "deadbeef"},
		"module_path":     {modulePath, "github.com/org/repo"},
		"kind":            {kind, "tracked"},
		"canonical_url":   {canonicalURL, "https://github.com/org/repo"},
		"identity_tier":   {gotTier, tier},
		"identity_anchor": {gotAnchor, anchor},
	}
	for col, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", col, c.got, c.want)
		}
	}
	if addedAt != now || lastAccessed != now || promptedAt != now {
		t.Errorf("integer columns not preserved: added=%d lastAccessed=%d prompted=%d (want %d)", addedAt, lastAccessed, promptedAt, now)
	}

	// Children moved.
	for _, c := range []struct {
		query string
		what  string
	}{
		{`SELECT count(*) FROM tasks WHERE repo_id=?`, "tasks"},
		{`SELECT count(*) FROM repo_aliases WHERE repo_id=?`, "repo_aliases"},
		{`SELECT count(*) FROM suppressions WHERE scope='repo' AND target=?`, "repo-suppressions"},
	} {
		var moved int
		mustScan(t, db, c.query, &moved, newID)
		if moved != 1 {
			t.Errorf("%s not moved to new id (count=%d)", c.what, moved)
		}
	}
}

// TestReschemeRepoIdentities_ReKeysToModuleTier exercises the pre-rescan step
// end to end against the real ResolveIdentity: a repo whose go.mod declares a
// host/path module re-keys from its legacy abs-root id to the module-hostpath
// tier id, and a repo-scope suppression follows.
func TestReschemeRepoIdentities_ReKeysToModuleTier(t *testing.T) {
	db := openReschemeDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module github.com/org/repo\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const legacyID = "legacy-abs-root-id"
	mustExec(t, db, `INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, legacyID, root, now)
	mustExec(t, db, `INSERT INTO suppressions (suppression_id, scope, target, reason, created_at, actor_id, actor_kind) VALUES (?,?,?,?,?,?,?)`,
		"sr", "repo", legacyID, "r", now, "tester", "human")

	repoMap, err := reschemeRepoIdentities(ctx, db, db)
	if err != nil {
		t.Fatalf("reschemeRepoIdentities: %v", err)
	}
	newID, ok := repoMap[legacyID]
	if !ok || newID == legacyID {
		t.Fatalf("repo not re-keyed: map=%v", repoMap)
	}

	var tier, anchor string
	mustScanRow(t, db, `SELECT identity_tier, identity_anchor FROM repos WHERE repo_id=?`, newID, &tier, &anchor)
	if tier != "module-hostpath" || anchor != "github.com/org/repo" {
		t.Errorf("tier/anchor = %q/%q, want module-hostpath/github.com/org/repo", tier, anchor)
	}
	var suppTarget string
	mustScanRow(t, db, `SELECT target FROM suppressions WHERE suppression_id=?`, "sr", &suppTarget)
	if suppTarget != newID {
		t.Errorf("repo suppression target = %q, want %q", suppTarget, newID)
	}
}

func mustScan(t *testing.T, db *sql.DB, query string, dst *int, args ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", query, err)
	}
}

func mustScanRow(t *testing.T, db *sql.DB, query string, arg any, dst ...any) {
	t.Helper()
	if err := db.QueryRow(query, arg).Scan(dst...); err != nil {
		t.Fatalf("scan row %q: %v", query, err)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func insertNode(t *testing.T, db *sql.DB, nodeID, repoID, filePath, kind, symbolPath string, now int64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO nodes
		(node_id, branch, repo_id, language, kind, symbol_path, file_path,
		 content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, "main", repoID, "go", kind, symbolPath, filePath,
		"hash-"+nodeID, now, "system", "system")
}

func insertFinding(t *testing.T, db *sql.DB, findingID, repoID, nodeID, rule string, now int64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO findings
		(finding_id, branch, repo_id, node_id, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		findingID, "main", repoID, nodeID, "low", "structural", rule, "m", "open", now, "system", "system")
}
