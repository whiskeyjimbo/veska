// SPDX-License-Identifier: AGPL-3.0-only

//go:build mcp_latency_bench

package main

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

func TestFindSymbolLatency_Sanity(t *testing.T) {
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

	const nNodes = 100
	if err := seedNodes(db, "repo-test", "main", nNodes); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Warm run
	rows, err := db.Query(findSymbolQuery, "repo-test", "main", "pkg.Symbol50")
	if err != nil {
		t.Fatalf("warm query: %v", err)
	}
	rows.Close()

	// 10 iterations - sanity only, gate is <1s per query
	const iters = 10
	deadline := 1 * time.Second
	for i := range iters {
		sym := fmt.Sprintf("pkg.Symbol%d", rand.IntN(nNodes))
		start := time.Now()
		r, err := db.Query(findSymbolQuery, "repo-test", "main", sym)
		if err != nil {
			t.Fatalf("iter %d query: %v", i, err)
		}
		r.Close()
		elapsed := time.Since(start)
		if elapsed > deadline {
			t.Errorf("iter %d: query took %v, want <%v", i, elapsed, deadline)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
