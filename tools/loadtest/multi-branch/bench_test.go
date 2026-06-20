// SPDX-License-Identifier: AGPL-3.0-only

//go:build multi_branch_bench

package main

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

func TestSeedAndQuery(t *testing.T) {
	dir := t.TempDir()
	dbPath := fmt.Sprintf("%s/test.db", dir)

	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := setupSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}

	const (
		nBranches = 2
		nNodes    = 50
		repoID    = "repo-test"
	)

	if err := seedRepo(db, repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	for b := range nBranches {
		branch := fmt.Sprintf("branch-%d", b)
		if err := seedBranchNodes(db, repoID, branch, nNodes); err != nil {
			t.Fatalf("seed branch %s: %v", branch, err)
		}
		if err := seedBranchEdges(db, repoID, branch, nNodes); err != nil {
			t.Fatalf("seed edges %s: %v", branch, err)
		}
		if err := seedBranchFindings(db, repoID, branch, nNodes); err != nil {
			t.Fatalf("seed findings %s: %v", branch, err)
		}
	}

	// Assert row counts.
	var nodeCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount != nBranches*nNodes {
		t.Errorf("want %d nodes, got %d", nBranches*nNodes, nodeCount)
	}

	var edgeCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount != nBranches*nNodes {
		t.Errorf("want %d edges, got %d", nBranches*nNodes, edgeCount)
	}

	// Run 10 query iterations - assert each < 1s.
	const iters = 10
	deadline := time.Second
	for i := range iters {
		nodeID := fmt.Sprintf("node-branch-0-%d", i%nNodes)
		start := time.Now()
		rows, err := db.Query(nodeQuery, repoID, "branch-0", nodeID)
		if err != nil {
			t.Fatalf("iter %d query: %v", i, err)
		}
		rows.Close()
		if elapsed := time.Since(start); elapsed > deadline {
			t.Errorf("iter %d: query took %v, want <%v", i, elapsed, deadline)
		}
	}
}

func TestPromotionTrial(t *testing.T) {
	dir := t.TempDir()
	dbPath := fmt.Sprintf("%s/promo.db", dir)

	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := setupSchema(db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if err := seedRepo(db, "repo-promo"); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	start := time.Now()
	if err := txInsert(db, "repo-promo", "promo-branch-0", 100); err != nil {
		t.Fatalf("txInsert: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("single promotion trial took %v, want <1s", elapsed)
	}

	// Verify rows inserted.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes WHERE branch=?", "promo-branch-0").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 100 {
		t.Errorf("want 100 nodes, got %d", count)
	}
}
