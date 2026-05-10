package pkloader_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/branchpk/pkloader"
)

func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?cache=shared&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open mem db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSchemaCreated(t *testing.T) {
	db := openMemDB(t)
	if err := pkloader.CreateSchema(db); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	tables := []string{"repos", "nodes", "edges", "findings"}
	for _, tbl := range tables {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", tbl, err)
		}
	}
}

func TestBaseSymbols(t *testing.T) {
	syms := pkloader.GenerateBaseSymbols(100, "repo1")
	if len(syms) != 100 {
		t.Fatalf("expected 100 symbols, got %d", len(syms))
	}
	// IDs must be stable (deterministic)
	syms2 := pkloader.GenerateBaseSymbols(100, "repo1")
	for i := range syms {
		if syms[i].NodeID != syms2[i].NodeID {
			t.Errorf("symbol[%d] NodeID not deterministic: %q vs %q", i, syms[i].NodeID, syms2[i].NodeID)
		}
		if syms[i].ContentHash != syms2[i].ContentHash {
			t.Errorf("symbol[%d] ContentHash not deterministic", i)
		}
	}
}

func TestDirtyOverlap(t *testing.T) {
	base := pkloader.GenerateBaseSymbols(100, "repo1")
	dirty := pkloader.ApplyDirtyOverlap(base, 10, 42)

	if len(dirty) != len(base) {
		t.Fatalf("dirty len %d != base len %d", len(dirty), len(base))
	}

	var changed, same int
	for i := range base {
		if dirty[i].ContentHash != base[i].ContentHash {
			changed++
		} else {
			same++
		}
	}
	if changed != 10 {
		t.Errorf("expected 10 changed content hashes for 10%% overlap, got %d", changed)
	}
	if same != 90 {
		t.Errorf("expected 90 unchanged, got %d", same)
	}
}

func TestInsertBranch(t *testing.T) {
	db := openMemDB(t)
	if err := pkloader.CreateSchema(db); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if err := pkloader.InsertRepo(db, "repo1"); err != nil {
		t.Fatalf("InsertRepo: %v", err)
	}

	syms := pkloader.GenerateBaseSymbols(200, "repo1")
	if err := pkloader.InsertBranch(db, "main", "repo1", syms, 1700000000); err != nil {
		t.Fatalf("InsertBranch: %v", err)
	}

	var nodeCount int64
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes WHERE branch='main'").Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount != 200 {
		t.Errorf("expected 200 node rows, got %d", nodeCount)
	}

	var edgeCount int64
	if err := db.QueryRow("SELECT COUNT(*) FROM edges WHERE branch='main'").Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount != 200 {
		t.Errorf("expected 200 edge rows, got %d", edgeCount)
	}

	var findingCount int64
	if err := db.QueryRow("SELECT COUNT(*) FROM findings WHERE branch='main'").Scan(&findingCount); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	// 1 finding per 100 symbols: 200/100 = 2
	if findingCount != 2 {
		t.Errorf("expected 2 finding rows, got %d", findingCount)
	}
}

func TestLoadMetricsJSON(t *testing.T) {
	m := pkloader.LoadMetrics{
		OverlapPct:   10,
		Branches:     50,
		Symbols:      100000,
		NodeRows:     5000000,
		EdgeRows:     5000000,
		FindingRows:  50000,
		DBBytes:      12345,
		WALBytes:     456,
		PeakRSSBytes: 789,
		LoadWallMs:   1234,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	requiredFields := []string{
		"overlap_pct", "branches", "symbols",
		"node_rows", "edge_rows", "finding_rows",
		"db_bytes", "wal_bytes", "peak_rss_bytes", "load_wall_ms",
	}
	s := string(b)
	for _, f := range requiredFields {
		if !containsField(s, f) {
			t.Errorf("JSON missing field %q: %s", f, s)
		}
	}

	// Round-trip
	var m2 pkloader.LoadMetrics
	if err := json.Unmarshal(b, &m2); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m2.OverlapPct != 10 || m2.Branches != 50 || m2.Symbols != 100000 {
		t.Errorf("round-trip mismatch: %+v", m2)
	}
}

func containsField(s, field string) bool {
	return len(s) > 0 && (jsonHasKey(s, `"`+field+`"`))
}

func jsonHasKey(s, key string) bool {
	for i := 0; i < len(s)-len(key); i++ {
		if s[i:i+len(key)] == key {
			return true
		}
	}
	return false
}
