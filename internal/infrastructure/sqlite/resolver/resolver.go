// Package resolver provides query-time cross-repo edge resolution for the
// solov2 graph store. It performs one-hop indexed lookups against the
// cross_repo_edge_stubs and nodes tables. Multi-hop traversal is deferred to M2+.
package resolver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CrossRepoStub carries the fields needed to resolve a single cross-repo edge
// stub at query time.
type CrossRepoStub struct {
	StubID     string
	SrcNodeID  string
	Kind       string
	ModulePath string
	SymbolPath string
	Language   string
}

// ResolvedEdge is the result of a successful one-hop cross-repo resolution.
// CrossRepo is always true for edges produced by this package.
type ResolvedEdge struct {
	SrcNodeID string
	DstNodeID string
	DstRepoID string
	DstBranch string
	Kind      string
	CrossRepo bool
}

// ResolveCrossRepoEdge performs a single indexed lookup to find the destination
// node for the given stub. It returns nil, nil when no matching node exists
// (silent miss). expandCrossRepo is accepted for API symmetry with M2+ multi-hop
// expansion; for now both values produce the same single-hop result.
func ResolveCrossRepoEdge(ctx context.Context, db *sql.DB, stub CrossRepoStub, expandCrossRepo bool) (*ResolvedEdge, error) {
	const q = `
		SELECT n.node_id, n.repo_id, n.branch
		FROM nodes n
		JOIN repos r ON r.repo_id = n.repo_id
		WHERE r.module_path = ?
		  AND n.symbol_path = ?
		  AND n.language    = ?
		  AND n.branch      = r.active_branch
		LIMIT 1`

	var dstNodeID, dstRepoID, dstBranch string
	err := db.QueryRowContext(ctx, q, stub.ModulePath, stub.SymbolPath, stub.Language).
		Scan(&dstNodeID, &dstRepoID, &dstBranch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // silent miss
		}
		return nil, fmt.Errorf("resolver: lookup stub %s: %w", stub.StubID, err)
	}

	return &ResolvedEdge{
		SrcNodeID: stub.SrcNodeID,
		DstNodeID: dstNodeID,
		DstRepoID: dstRepoID,
		DstBranch: dstBranch,
		Kind:      stub.Kind,
		CrossRepo: true,
	}, nil
}

// ResolveStubsForNode fetches all cross_repo_edge_stubs whose src_node_id
// matches nodeID on the given branch, then resolves each with a one-hop lookup.
// Stubs that do not resolve to an indexed node are silently dropped.
// expandCrossRepo is forwarded to ResolveCrossRepoEdge (M2+ extension point).
func ResolveStubsForNode(ctx context.Context, db *sql.DB, nodeID, branch string, expandCrossRepo bool) ([]ResolvedEdge, error) {
	const q = `
		SELECT stub_id, src_node_id, kind, module_path, symbol_path, language
		FROM cross_repo_edge_stubs
		WHERE src_node_id = ? AND branch = ?`

	rows, err := db.QueryContext(ctx, q, nodeID, branch)
	if err != nil {
		return nil, fmt.Errorf("resolver: fetch stubs for node %s@%s: %w", nodeID, branch, err)
	}
	defer rows.Close()

	var stubs []CrossRepoStub
	for rows.Next() {
		var s CrossRepoStub
		if err := rows.Scan(&s.StubID, &s.SrcNodeID, &s.Kind, &s.ModulePath, &s.SymbolPath, &s.Language); err != nil {
			return nil, fmt.Errorf("resolver: scan stub row: %w", err)
		}
		stubs = append(stubs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolver: iterate stubs: %w", err)
	}

	var edges []ResolvedEdge
	for _, stub := range stubs {
		edge, err := ResolveCrossRepoEdge(ctx, db, stub, expandCrossRepo)
		if err != nil {
			return nil, err
		}
		if edge == nil {
			continue // silent miss
		}
		edges = append(edges, *edge)
	}
	return edges, nil
}
