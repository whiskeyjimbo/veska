package daemon

// rescheme.go completes the ADR-S0017 atomic node-identity migration that
// SQL migration 0019 begins. 0019 snapshots the pre-migration graph into
// _dchd_old_* tables, drops the derived graph, and clears last_promoted_sha so
// the startup resync full-reparses every repo. This file is the Go half — the
// parts that need go.mod/git inspection (portable repo identity) and a join
// against the repopulated graph (suppression carry-forward), neither of which a
// pure-SQL migration can express.
//
// Sequence (solov2-dchd.4 + .5):
//  1. pre-rescan: re-resolve each repo's portable identity (ADR-S0017 §2) and
//     re-key repo_id so the rescan scans under the new id;
//  2. rescan: StartupResync full-reparses every repo (last_promoted_sha == "");
//  3. post-rescan: carry user suppressions forward across the re-key by joining
//     the _dchd_old_* snapshot to the repopulated graph, reporting anything that
//     cannot be deterministically remapped (ADR-S0017's acceptance gate);
//  4. drop the snapshot tables — the durable "done" marker.
//
// Idempotent: a crash before step 4 leaves the snapshot tables, so the next
// boot re-runs. Per-repo re-key is guarded on the old id still being present in
// repos, so a re-run skips already-migrated repos.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// reschemeIdentity runs the full ADR-S0017 rescheme when migration 0019 is
// pending; otherwise it is a no-op. It drives steps 1–4 above and returns the
// first error encountered (callers fall through to a normal resync on error,
// since a half-applied rescheme still left the graph needing a rebuild).
func (d *Daemon) reschemeIdentity(ctx context.Context) error {
	state, err := reschemeState(ctx, d.pools.ReadDB)
	if err != nil {
		return fmt.Errorf("rescheme: detect pending: %w", err)
	}
	switch state {
	case reschemeNone:
		return nil
	case reschemeEmpty:
		// Migration 0019 ran on a DB with no pre-existing graph (fresh install,
		// or a test DB): the snapshot tables exist but are empty, so there is
		// nothing to carry forward. Drop them so they don't linger or re-trigger.
		return dropReschemeSnapshots(ctx, d.pools.Write)
	}
	slog.Info("identity rescheme: ADR-S0017 migration detected; re-keying graph identity")

	repoMap, err := reschemeRepoIdentities(ctx, d.pools.ReadDB, d.pools.Write)
	if err != nil {
		return fmt.Errorf("rescheme: repo re-key: %w", err)
	}

	// Synchronous rescan: repopulate the graph under the new identity scheme
	// BEFORE the suppression remap, which joins against the repopulated nodes.
	if err := d.resync.Run(ctx); err != nil {
		return fmt.Errorf("rescheme: rescan: %w", err)
	}

	report, err := reschemeSuppressions(ctx, d.pools.ReadDB, d.pools.Write, repoMap)
	if err != nil {
		return fmt.Errorf("rescheme: suppressions: %w", err)
	}
	report.log()

	if err := dropReschemeSnapshots(ctx, d.pools.Write); err != nil {
		return fmt.Errorf("rescheme: drop snapshots: %w", err)
	}
	slog.Info("identity rescheme: complete",
		"repos_rekeyed", repoMap.changed(),
		"suppressions_remapped", report.remapped,
		"suppressions_unremappable", len(report.unremappable),
	)
	return nil
}

// reschemeStatus classifies the 0019 migration's post-state.
type reschemeStatus int

const (
	// reschemeNone: no snapshot tables — rescheme already done, or 0019 not run.
	reschemeNone reschemeStatus = iota
	// reschemeEmpty: snapshot tables exist but hold no pre-migration graph
	// (fresh install / test DB). Nothing to carry forward; just clean up.
	reschemeEmpty
	// reschemePending: snapshot tables hold a real pre-migration graph that
	// must be re-keyed and have its suppressions carried forward.
	reschemePending
)

// reschemeState inspects whether migration 0019's snapshot tables are present
// and whether they captured a real pre-migration graph. Gating on a NON-EMPTY
// snapshot (not mere existence) is essential: 0019 unconditionally CREATEs the
// _dchd_old_* tables, so on a fresh install they exist but are empty — firing a
// full rescheme (which re-resolves and re-keys every repo) on every fresh DB
// would corrupt synthetic-id test fixtures and waste a boot.
func reschemeState(ctx context.Context, db *sql.DB) (reschemeStatus, error) {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_dchd_old_nodes'`,
	).Scan(&present); err != nil {
		return reschemeNone, err
	}
	if present == 0 {
		return reschemeNone, nil
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM _dchd_old_nodes`).Scan(&rows); err != nil {
		return reschemeNone, err
	}
	if rows == 0 {
		return reschemeEmpty, nil
	}
	return reschemePending, nil
}

// repoIDMap maps every pre-migration repo_id to its post-migration repo_id.
// Unchanged repos map to themselves (abs-root tier, or already-tiered repos
// added after 0018).
type repoIDMap map[string]string

// changed counts repos whose id actually moved.
func (m repoIDMap) changed() int {
	n := 0
	for old, new := range m {
		if old != new {
			n++
		}
	}
	return n
}

// reschemeRepoIdentities re-resolves the portable identity of every repo and
// re-keys repo_id where the resolved id differs. It returns the old->new map
// keyed on PRE-migration repo_ids, which the node remap and suppression
// carry-forward both join against.
//
// The map is built from the _dchd_old_repos SNAPSHOT, not the live repos table:
// the live table is what this function mutates, so reading it on a crash-recovery
// re-run (after the re-key but before the snapshot drop) would key the map on the
// already-NEW ids, and buildNodeRemap — which looks up old node repo_ids — would
// then miss every node and silently drop the suppression carry-forward. Sourcing
// the stable snapshot ids makes the map deterministic across re-runs.
//
// The re-key itself is idempotent: it runs only while the live row still holds
// the OLD id, so a recovery re-run (row already at the new id) skips it.
func reschemeRepoIdentities(ctx context.Context, read, write *sql.DB) (repoIDMap, error) {
	rows, err := read.QueryContext(ctx, `SELECT repo_id, root_path FROM _dchd_old_repos`)
	if err != nil {
		return nil, fmt.Errorf("list snapshot repos: %w", err)
	}
	type repoRow struct{ oldID, root string }
	var repos []repoRow
	for rows.Next() {
		var r repoRow
		if err := rows.Scan(&r.oldID, &r.root); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make(repoIDMap, len(repos))
	for _, r := range repos {
		tier, anchor, newID := repo.ResolveIdentity(ctx, r.root)
		out[r.oldID] = newID

		// Idempotency: only act while the live row still holds the old id. On a
		// recovery re-run it already holds newID, so both branches no-op.
		stillOld, err := rowExists(ctx, read, `SELECT 1 FROM repos WHERE repo_id=? LIMIT 1`, r.oldID)
		if err != nil {
			return nil, err
		}
		if !stillOld {
			continue
		}
		if newID == r.oldID {
			// Identity unchanged (abs-root tier, or already tiered). Backfill
			// tier/anchor in case they were NULL (pre-0018 abs-root rows).
			if _, err := write.ExecContext(ctx,
				`UPDATE repos SET identity_tier=?, identity_anchor=? WHERE repo_id=?`,
				string(tier), anchor, r.oldID,
			); err != nil {
				return nil, fmt.Errorf("backfill tier for %s: %w", r.oldID, err)
			}
			continue
		}
		if err := rekeyRepoID(ctx, write, r.oldID, newID, string(tier), anchor); err != nil {
			return nil, err
		}
		slog.Info("identity rescheme: repo re-keyed",
			"root", r.root, "tier", tier, "old_repo_id", r.oldID, "new_repo_id", newID)
	}
	return out, nil
}

// rekeyRepoID moves a repo from oldID to newID (with the resolved tier/anchor)
// inside a single transaction. It errors rather than clobbering if newID
// already names a different repos row (two repos resolving to one identity).
//
// The re-key is an IN-PLACE PK UPDATE, not an INSERT-new/DELETE-old copy:
// repos.root_path (and partially canonical_url) is UNIQUE, so two coexisting
// rows for one repo would violate that constraint. An in-place PK update keeps
// a single row, and `PRAGMA defer_foreign_keys=ON` holds the ON-DELETE-CASCADE
// child FKs (tasks, repo_aliases — others are empty after migration 0019) until
// commit, so the parent and its repointed children are validated together at
// commit rather than mid-statement. defer_foreign_keys resets at end of tx.
func rekeyRepoID(ctx context.Context, write *sql.DB, oldID, newID, tier, anchor string) error {
	tx, err := write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin re-key tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	var collide int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM repos WHERE repo_id=?`, newID,
	).Scan(&collide); err != nil {
		return fmt.Errorf("collision check %s: %w", newID, err)
	}
	if collide > 0 {
		return fmt.Errorf("re-key %s -> %s: target id already registered (two repos share an identity anchor)", oldID, newID)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE repos SET repo_id=?, identity_tier=?, identity_anchor=? WHERE repo_id=?`,
		newID, tier, anchor, oldID,
	); err != nil {
		return fmt.Errorf("update repos id %s -> %s: %w", oldID, newID, err)
	}

	for _, stmt := range []struct {
		sql  string
		what string
	}{
		{`UPDATE tasks SET repo_id=? WHERE repo_id=?`, "tasks"},
		{`UPDATE repo_aliases SET repo_id=? WHERE repo_id=?`, "repo_aliases"},
		{`UPDATE suppressions SET target=? WHERE scope='repo' AND target=?`, "repo-suppressions"},
	} {
		if _, err := tx.ExecContext(ctx, stmt.sql, newID, oldID); err != nil {
			return fmt.Errorf("repoint %s %s -> %s: %w", stmt.what, oldID, newID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit re-key %s -> %s: %w", oldID, newID, err)
	}
	return nil
}

// dropReschemeSnapshots removes the 0019 snapshot tables once the remap has
// committed. This is the durable done-marker: its absence on the next boot is
// what makes reschemeIdentity a no-op.
func dropReschemeSnapshots(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"_dchd_old_nodes", "_dchd_old_findings", "_dchd_old_repos"} {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

// relStoredPath normalises a snapshot file_path to the repo-relative slash form
// the rescan stores. An absolute path is relativised against root; a path that
// is ALREADY relative (e.g. findings stored file_path "go.mod" even pre-ADR) is
// returned ToSlash'd as-is. Unlike relativizeOldPath it always yields a value —
// used where the snapshot path may be either form (finding file anchors).
func relStoredPath(root, p string) string {
	if filepath.IsAbs(p) {
		if rel, err := filepath.Rel(root, p); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// relativizeOldPath converts a snapshot's absolute file_path to the repo-relative
// slash form the rescan now stores, given the repo's pre-migration root. Returns
// ("", false) when the path is not under root (defensive — never expected for a
// node that was scanned under that root).
func relativizeOldPath(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return "", false
	}
	return rel, true
}
