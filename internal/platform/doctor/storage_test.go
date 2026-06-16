package doctor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// writeSidecar writes a minimal JSON sidecar with the given number of rows
// into dir under the given base name (e.g. "vec-r1|main|nomic").
// We write the sidecar manually — no dependency on the hnsw_native package.
func writeSidecar(t *testing.T, dir, base string, rowCount int) {
	t.Helper()
	// Build a map with rowCount entries. Keys are uint64 marshalled as strings.
	rows := make(map[string]json.RawMessage, rowCount)
	for i := range rowCount {
		rows[string(rune('0'+i))] = json.RawMessage(`{}`)
	}
	data, err := json.Marshal(map[string]any{
		"rows": rows,
	})
	if err != nil {
		t.Fatalf("writeSidecar: marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), data, 0o644); err != nil {
		t.Fatalf("writeSidecar: write: %v", err)
	}
}

// writeHNSW writes a fake.hnsw file of exactly size bytes.
func writeHNSW(t *testing.T, dir, base string, size int) {
	t.Helper()
	data := make([]byte, size)
	if err := os.WriteFile(filepath.Join(dir, base+".hnsw"), data, 0o644); err != nil {
		t.Fatalf("writeHNSW: write: %v", err)
	}
}

// TestCheckStorage_EmptyDir verifies that an empty directory yields zero HNSW
// metrics and a correctly computed FreeRatio.
func TestCheckStorage_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	report, err := doctor.CheckStorage(dir)
	if err != nil {
		t.Fatalf("CheckStorage: unexpected error: %v", err)
	}

	if report.VeskaHome != dir {
		t.Errorf("VeskaHome: got %q, want %q", report.VeskaHome, dir)
	}
	if report.HNSWSizeBytes != 0 {
		t.Errorf("HNSWSizeBytes: got %d, want 0", report.HNSWSizeBytes)
	}
	if report.HNSWVectorCount != 0 {
		t.Errorf("HNSWVectorCount: got %d, want 0", report.HNSWVectorCount)
	}
	if len(report.HNSWIndexPaths) != 0 {
		t.Errorf("HNSWIndexPaths: got %v, want []", report.HNSWIndexPaths)
	}
	// FreeRatio with no DB/WAL/HNSW: denominator = FreeBytes.
	// If FreeBytes > 0, ratio should be 1.0.
	// If FreeBytes == 0 (edge case), ratio should be 1.0 (div-zero guard).
	if report.FreeBytes > 0 && report.FreeRatio != 1.0 {
		t.Errorf("FreeRatio: got %f, want 1.0 (no db/wal/hnsw)", report.FreeRatio)
	}
	if report.FreeBytes == 0 && report.FreeRatio != 1.0 {
		t.Errorf("FreeRatio: got %f, want 1.0 (zero denominator guard)", report.FreeRatio)
	}
}

// TestCheckStorage_WithSidecars writes two.hnsw + two.json sidecar files and
// asserts that sizes and row counts are summed correctly.
func TestCheckStorage_WithSidecars(t *testing.T) {
	dir := t.TempDir()

	const (
		base1 = "vec-r1|main|nomic"
		base2 = "vec-r2|feat|nomic"
		size1 = 1000
		size2 = 2048
		rows1 = 3
		rows2 = 5
	)

	writeHNSW(t, dir, base1, size1)
	writeHNSW(t, dir, base2, size2)
	writeSidecar(t, dir, base1, rows1)
	writeSidecar(t, dir, base2, rows2)

	report, err := doctor.CheckStorage(dir)
	if err != nil {
		t.Fatalf("CheckStorage: unexpected error: %v", err)
	}

	if report.HNSWSizeBytes != size1+size2 {
		t.Errorf("HNSWSizeBytes: got %d, want %d", report.HNSWSizeBytes, size1+size2)
	}
	if report.HNSWVectorCount != rows1+rows2 {
		t.Errorf("HNSWVectorCount: got %d, want %d", report.HNSWVectorCount, rows1+rows2)
	}
	if len(report.HNSWIndexPaths) != 2 {
		t.Errorf("HNSWIndexPaths len: got %d, want 2", len(report.HNSWIndexPaths))
	}

	// Paths should be sorted.
	sorted := make([]string, len(report.HNSWIndexPaths))
	copy(sorted, report.HNSWIndexPaths)
	sort.Strings(sorted)
	for i, p := range sorted {
		if report.HNSWIndexPaths[i] != p {
			t.Errorf("HNSWIndexPaths not sorted at index %d", i)
		}
	}
}

// TestStorageReport_JSONKeys marshals a StorageReport and asserts that every
// required JSON key from the schema is present.
func TestStorageReport_JSONKeys(t *testing.T) {
	r := doctor.StorageReport{
		VeskaHome:       "/home/jeff/.veska",
		DBPath:          "/home/jeff/.veska/veska.db",
		DBSizeBytes:     1287654321,
		WALSizeBytes:    8388608,
		HNSWIndexPaths:  []string{"/home/jeff/.veska/vec-r1|main|nomic.hnsw"},
		HNSWSizeBytes:   411261952,
		HNSWVectorCount: 250000,
		FreeBytes:       442891534336,
		FreeRatio:       0.81,
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	required := []string{
		"veska_home",
		"db_path",
		"db_size_bytes",
		"wal_size_bytes",
		"hnsw_index_paths",
		"hnsw_size_bytes",
		"hnsw_vector_count",
		"free_bytes",
		"free_ratio",
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON key %q missing from marshalled StorageReport", key)
		}
	}
}

// TestFreeRatio_ZeroDenominator verifies the div-by-zero guard:
// if all sizes and FreeBytes are 0, FreeRatio must be 1.0.
func TestFreeRatio_ZeroDenominator(t *testing.T) {
	dir := t.TempDir()

	// Override: we can't force FreeBytes to 0 via the real filesystem call, so
	// we call the exported helper directly instead.
	ratio := doctor.ComputeFreeRatio(0, 0, 0, 0)
	if ratio != 1.0 {
		t.Errorf("ComputeFreeRatio(0,0,0,0): got %f, want 1.0", ratio)
	}

	// Non-zero denominator sanity check.
	// free=100, db=50, wal=25, hnsw=25 → total=200, ratio=0.5
	ratio2 := doctor.ComputeFreeRatio(100, 50, 25, 25)
	if ratio2 != 0.5 {
		t.Errorf("ComputeFreeRatio(100,50,25,25): got %f, want 0.5", ratio2)
	}

	_ = dir // dir used only to confirm tempdir creation succeeded
}
