package daemon

// rescheme_suppressions.go is the ADR-S0017 suppression carry-forward
// (solov2-dchd.5). Suppressions are user-authored data that does NOT self-heal:
// every scope keys on something the identity migration moves —
//
//	symbol  : target = node_id            (re-keyed: repoID + path moved)
//	finding : target = finding_id         (hash of rule + node_id anchor)
//	file    : target = file path          (absolute -> repo-relative)
//	repo    : target = repo_id            (handled in rescheme.go's re-key)
//
// The remap runs AFTER the rescan so it can join the _dchd_old_* snapshot to the
// repopulated graph by real, post-rescan ids. Anything that cannot be
// deterministically remapped is REPORTED, not silently dropped — that is the
// ADR's acceptance gate. The classic non-remappable case is a finding whose
// finding_id folded a WithFindingKey discriminator (not persisted), so the
// rule+anchor lookup returns several findings: it is reported, never guessed.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
)

// unremappableSuppression records a suppression that survived the migration but
// whose target could not be deterministically re-keyed. Surfaced (not deleted)
// so the operator can re-author it.
type unremappableSuppression struct {
	suppressionID string
	scope         string
	oldTarget     string
	reason        string
}

// reschemeReport accumulates the suppression carry-forward outcome.
type reschemeReport struct {
	remapped     int
	unremappable []unremappableSuppression
}

func (r *reschemeReport) miss(id, scope, target, reason string) {
	r.unremappable = append(r.unremappable, unremappableSuppression{id, scope, target, reason})
}

// log emits the report: an INFO line for the count, plus a WARN per
// non-remappable suppression so they are visible in the daemon log and a
// `veska doctor` reader could surface them later.
func (r *reschemeReport) log() {
	slog.Info("identity rescheme: suppressions carried forward",
		"remapped", r.remapped, "unremappable", len(r.unremappable))
	for _, u := range r.unremappable {
		slog.Warn("identity rescheme: suppression could not be remapped; re-author it",
			"suppression_id", u.suppressionID,
			"scope", u.scope,
			"old_target", u.oldTarget,
			"reason", u.reason,
		)
	}
}

// reschemeSuppressions carries every suppression scope forward across the
// re-key, using the repo map from the pre-rescan re-key and the _dchd_old_*
// snapshot. repo-scope suppressions were already moved during the repo re-key,
// so only symbol/finding/file are handled here.
func reschemeSuppressions(ctx context.Context, read, write *sql.DB, repoMap repoIDMap) (*reschemeReport, error) {
	nodeMap, err := buildNodeRemap(ctx, read, repoMap)
	if err != nil {
		return nil, fmt.Errorf("build node remap: %w", err)
	}
	rep := &reschemeReport{}
	if err := remapSymbolSuppressions(ctx, read, write, nodeMap, rep); err != nil {
		return nil, fmt.Errorf("symbol scope: %w", err)
	}
	if err := remapFindingSuppressions(ctx, read, write, repoMap, nodeMap, rep); err != nil {
		return nil, fmt.Errorf("finding scope: %w", err)
	}
	if err := remapFileSuppressions(ctx, read, write, rep); err != nil {
		return nil, fmt.Errorf("file scope: %w", err)
	}
	return rep, nil
}

// buildNodeRemap maps every pre-migration node_id to its post-rescan node_id by
// joining the snapshot to the repopulated graph on the node_id preimage
// (new repo_id, repo-relative path, kind, symbol_path). symbol_path is the
// parser node name folded into node_id (promotion_store maps n.Name ->
// symbol_path), so that tuple is unique by construction — the join is exact.
// A snapshot node with no post-rescan match (source changed since the original
// scan) is simply absent from the map; it only matters if a suppression targets
// it, in which case the scope remap reports the miss.
func buildNodeRemap(ctx context.Context, read *sql.DB, repoMap repoIDMap) (map[string]string, error) {
	roots, err := oldRepoRoots(ctx, read)
	if err != nil {
		return nil, err
	}
	rows, err := read.QueryContext(ctx,
		`SELECT node_id, repo_id, file_path, kind, symbol_path FROM _dchd_old_nodes`)
	if err != nil {
		return nil, fmt.Errorf("read old nodes: %w", err)
	}
	type oldNode struct{ nodeID, repoID, absPath, kind, symbolPath string }
	var olds []oldNode
	for rows.Next() {
		var o oldNode
		if err := rows.Scan(&o.nodeID, &o.repoID, &o.absPath, &o.kind, &o.symbolPath); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan old node: %w", err)
		}
		olds = append(olds, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per-node lookups run AFTER the cursor is drained — a nested query inside
	// the open rows would deadlock on the write pool's single connection.
	out := map[string]string{}
	for _, o := range olds {
		root, ok := roots[o.repoID]
		if !ok {
			continue
		}
		rel, ok := relativizeOldPath(root, o.absPath)
		if !ok {
			continue
		}
		var newNodeID string
		err := read.QueryRowContext(ctx,
			`SELECT node_id FROM nodes WHERE repo_id=? AND kind=? AND file_path=? AND symbol_path=? LIMIT 1`,
			repoMap[o.repoID], o.kind, rel, o.symbolPath,
		).Scan(&newNodeID)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lookup new node for %s: %w", o.nodeID, err)
		}
		out[o.nodeID] = newNodeID
	}
	return out, nil
}

// oldRepoRoots indexes the pre-migration repo roots by old repo_id, for
// relativising snapshot file paths.
func oldRepoRoots(ctx context.Context, read *sql.DB) (map[string]string, error) {
	rows, err := read.QueryContext(ctx, `SELECT repo_id, root_path FROM _dchd_old_repos`)
	if err != nil {
		return nil, fmt.Errorf("read old repos: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, root string
		if err := rows.Scan(&id, &root); err != nil {
			return nil, fmt.Errorf("scan old repo: %w", err)
		}
		out[id] = root
	}
	return out, rows.Err()
}

// remapSymbolSuppressions re-keys symbol-scope suppressions (target = node_id).
func remapSymbolSuppressions(ctx context.Context, read, write *sql.DB, nodeMap map[string]string, rep *reschemeReport) error {
	rows, err := read.QueryContext(ctx,
		`SELECT suppression_id, target FROM suppressions WHERE scope='symbol'`)
	if err != nil {
		return err
	}
	type pair struct{ id, target string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.target); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		newID, ok := nodeMap[p.target]
		if !ok {
			// Not a remappable old id. If the target already names a current
			// node, it is a post-migration suppression (or an already-completed
			// remap) — leave it. Otherwise it is genuinely dangling.
			cur, err := rowExists(ctx, read, `SELECT 1 FROM nodes WHERE node_id=? LIMIT 1`, p.target)
			if err != nil {
				return err
			}
			if !cur {
				rep.miss(p.id, "symbol", p.target, "no node with the same (path, kind, symbol) exists after rescan")
			}
			continue
		}
		if _, err := write.ExecContext(ctx,
			`UPDATE suppressions SET target=? WHERE suppression_id=?`, newID, p.id); err != nil {
			return err
		}
		rep.remapped++
	}
	return nil
}

// rowExists reports whether query (a `SELECT 1 ... LIMIT 1`) returns a row.
func rowExists(ctx context.Context, read *sql.DB, query string, args ...any) (bool, error) {
	var one int
	err := read.QueryRowContext(ctx, query, args...).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// remapFindingSuppressions re-keys finding-scope suppressions (target =
// finding_id). The lost WithFindingKey discriminator makes a direct recompute
// impossible, so the remap is key-agnostic: resolve the old finding's
// (rule, anchor), map the anchor to its new id, then look up the post-rescan
// finding(s) matching (new repo_id, rule, new anchor). Exactly one match is the
// new finding_id; zero or several are reported (several == the WithFindingKey
// multi-finding-per-anchor case the ADR names).
func remapFindingSuppressions(ctx context.Context, read, write *sql.DB, repoMap repoIDMap, nodeMap map[string]string, rep *reschemeReport) error {
	rows, err := read.QueryContext(ctx,
		`SELECT suppression_id, target FROM suppressions WHERE scope='finding'`)
	if err != nil {
		return err
	}
	type pair struct{ id, target string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.target); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	roots, err := oldRepoRoots(ctx, read)
	if err != nil {
		return err
	}

	for _, p := range todo {
		// Resolve the old finding's anchor from the snapshot.
		var repoID, branch, rule string
		var oldNodeID, oldFilePath sql.NullString
		err := read.QueryRowContext(ctx,
			`SELECT repo_id, branch, node_id, file_path, rule FROM _dchd_old_findings WHERE finding_id=? LIMIT 1`,
			p.target,
		).Scan(&repoID, &branch, &oldNodeID, &oldFilePath, &rule)
		if err == sql.ErrNoRows {
			// Not a pre-migration finding_id. If it already names a current
			// finding, it is a post-migration suppression (or a completed
			// remap) — leave it; else it is genuinely dangling.
			cur, exErr := rowExists(ctx, read, `SELECT 1 FROM findings WHERE finding_id=? LIMIT 1`, p.target)
			if exErr != nil {
				return exErr
			}
			if !cur {
				rep.miss(p.id, "finding", p.target, "no finding with this id in the pre-migration snapshot or current graph")
			}
			continue
		}
		if err != nil {
			return err
		}

		newRepoID := repoMap[repoID]
		var newFindingID string
		var lookErr error
		switch {
		case oldNodeID.Valid && oldNodeID.String != "":
			newNodeID, ok := nodeMap[oldNodeID.String]
			if !ok {
				rep.miss(p.id, "finding", p.target, "anchor node no longer exists after rescan")
				continue
			}
			newFindingID, lookErr = uniqueFindingID(ctx, read,
				`SELECT finding_id FROM findings WHERE repo_id=? AND rule=? AND node_id=?`,
				newRepoID, rule, newNodeID)
		case oldFilePath.Valid && oldFilePath.String != "":
			// Finding file anchors may be stored relative already (e.g.
			// vuln-scan anchors on "go.mod"), so normalise either form.
			rel := relStoredPath(roots[repoID], oldFilePath.String)
			// finding_id = hash(rule, anchor, key). When the anchor AND repo are
			// both invariant under the migration (a relative file anchor in a
			// repo whose id did not change), the finding_id is unchanged, so the
			// suppression already targets the right id — leave it untouched.
			// Crucially this must not depend on the finding being present yet:
			// re-derivation can be async (vuln-scan runs off the OSV refresh),
			// so a recompute-and-match would falsely report it as unremappable.
			if rel == oldFilePath.String && newRepoID == repoID {
				continue
			}
			newFindingID, lookErr = uniqueFindingID(ctx, read,
				`SELECT finding_id FROM findings WHERE repo_id=? AND rule=? AND file_path=?`,
				newRepoID, rule, rel)
		default:
			rep.miss(p.id, "finding", p.target, "snapshot finding has neither node nor file anchor")
			continue
		}

		switch {
		case lookErr == errNoUniqueFinding:
			rep.miss(p.id, "finding", p.target, "rule+anchor matches zero or multiple findings after rescan (likely a keyed finding; discriminator key is not persisted)")
		case lookErr != nil:
			return lookErr
		default:
			if _, err := write.ExecContext(ctx,
				`UPDATE suppressions SET target=? WHERE suppression_id=?`, newFindingID, p.id); err != nil {
				return err
			}
			rep.remapped++
		}
	}
	return nil
}

// errNoUniqueFinding signals that a (repo, rule, anchor) lookup matched zero or
// more than one finding — the non-deterministic case the remap must report.
var errNoUniqueFinding = fmt.Errorf("no unique finding for rule+anchor")

// uniqueFindingID returns the single finding_id matching query+args, or
// errNoUniqueFinding when the match count is not exactly one.
func uniqueFindingID(ctx context.Context, read *sql.DB, query string, args ...any) (string, error) {
	rows, err := read.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", errNoUniqueFinding
	}
	return ids[0], nil
}

// remapFileSuppressions re-keys file-scope suppressions (target = file path).
// The suppressions table carries no repo_id, so the repo is inferred by which
// pre-migration root the absolute target sits under. A target that is already
// repo-relative is a post-migration suppression (or a completed remap) and is
// left untouched; an absolute target under no known root cannot be safely
// repointed and is reported.
func remapFileSuppressions(ctx context.Context, read, write *sql.DB, rep *reschemeReport) error {
	rows, err := read.QueryContext(ctx,
		`SELECT suppression_id, target FROM suppressions WHERE scope='file'`)
	if err != nil {
		return err
	}
	type pair struct{ id, target string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.target); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	roots, err := oldRepoRoots(ctx, read)
	if err != nil {
		return err
	}
	for _, p := range todo {
		if !filepath.IsAbs(p.target) {
			// Already repo-relative — post-migration / already current. Leave it.
			continue
		}
		rel, ok := matchRootRelativize(p.target, roots)
		if !ok {
			rep.miss(p.id, "file", p.target, "absolute target is not under any known repo root")
			continue
		}
		if _, err := write.ExecContext(ctx,
			`UPDATE suppressions SET target=? WHERE suppression_id=?`, rel, p.id); err != nil {
			return err
		}
		rep.remapped++
	}
	return nil
}

// matchRootRelativize finds the repo root that contains abs and returns the
// repo-relative slash path. Returns ("", false) when no root is a prefix.
func matchRootRelativize(abs string, roots map[string]string) (string, bool) {
	for _, root := range roots {
		if rel, ok := relativizeOldPath(root, abs); ok {
			return rel, true
		}
	}
	return "", false
}
