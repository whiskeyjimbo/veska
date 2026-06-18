// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package loader_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func TestOpen_CreatesVecNodesTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	l, err := loader.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	exists, err := l.TableExists("vec_nodes")
	if err != nil {
		t.Fatalf("TableExists: %v", err)
	}
	if !exists {
		t.Fatal("expected vec_nodes virtual table to exist after Open")
	}
}

func TestInsertBatch_RowCountMatches(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	l, err := loader.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	const n = 100
	vecs := gen.GenerateVectors(n, 42)

	if err := l.InsertBatch(vecs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	count, err := l.RowCount()
	if err != nil {
		t.Fatalf("RowCount: %v", err)
	}
	if count != n {
		t.Fatalf("RowCount: got %d, want %d", count, n)
	}
}

func TestDiskBytes_PositiveAfterInsertAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	l, err := loader.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	vecs := gen.GenerateVectors(500, 99)
	if err := l.InsertBatch(vecs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	bytes, err := l.DiskBytes()
	if err != nil {
		t.Fatalf("DiskBytes: %v", err)
	}
	if bytes <= 0 {
		t.Fatalf("DiskBytes: expected positive, got %d", bytes)
	}
}

func TestLoadMetrics_JSONShape(t *testing.T) {
	m := loader.LoadMetrics{
		Population:   50000,
		LoadWallMs:   1234,
		DiskBytes:    56789,
		PeakRSSBytes: 98765,
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	required := []string{"population", "load_wall_ms", "disk_bytes", "peak_rss_bytes"}
	for _, key := range required {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing JSON key: %q", key)
		}
	}

	if v, ok := decoded["population"].(float64); !ok || int64(v) != 50000 {
		t.Errorf("population: got %v, want 50000", decoded["population"])
	}
	if v, ok := decoded["load_wall_ms"].(float64); !ok || int64(v) != 1234 {
		t.Errorf("load_wall_ms: got %v, want 1234", decoded["load_wall_ms"])
	}
}

func TestInsertBatch_MultipleCallsAccumulate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	l, err := loader.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	batch1 := gen.GenerateVectors(50, 1)
	batch2 := gen.GenerateVectors(75, 2)

	if err := l.InsertBatch(batch1); err != nil {
		t.Fatalf("InsertBatch batch1: %v", err)
	}
	if err := l.InsertBatch(batch2); err != nil {
		t.Fatalf("InsertBatch batch2: %v", err)
	}

	count, err := l.RowCount()
	if err != nil {
		t.Fatalf("RowCount: %v", err)
	}
	if count != 125 {
		t.Fatalf("RowCount: got %d, want 125", count)
	}
}

func TestRSSBytes_NonNegative(t *testing.T) {
	rss := loader.ReadRSSBytes()
	if rss < 0 {
		t.Fatalf("ReadRSSBytes: got %d, expected >= 0", rss)
	}
}

func TestMetrics_OutputFile(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "load_metrics.json")

	metrics := []loader.LoadMetrics{
		{Population: 50000, LoadWallMs: 100, DiskBytes: 1024, PeakRSSBytes: 2048},
		{Population: 1000000, LoadWallMs: 2000, DiskBytes: 20480, PeakRSSBytes: 40960},
	}

	if err := loader.WriteMetricsJSON(outPath, metrics); err != nil {
		t.Fatalf("WriteMetricsJSON: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 metric objects, got %d", len(decoded))
	}
	if v, ok := decoded[0]["population"].(float64); !ok || int64(v) != 50000 {
		t.Errorf("decoded[0].population: got %v, want 50000", decoded[0]["population"])
	}
	if v, ok := decoded[1]["population"].(float64); !ok || int64(v) != 1000000 {
		t.Errorf("decoded[1].population: got %v, want 1000000", decoded[1]["population"])
	}
}
