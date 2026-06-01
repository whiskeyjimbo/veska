package doctor_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// TestCheckStorageBackend_Memory_Empty verifies the report for an empty in-memory store.
func TestCheckStorageBackend_Memory_Empty(t *testing.T) {
	store := memvec.New()
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: store.TotalVectorCount(),
	})

	if report.Backend != "memory" {
		t.Errorf("Backend: got %q, want %q", report.Backend, "memory")
	}
	if report.VectorCount != 0 {
		t.Errorf("VectorCount: got %d, want 0", report.VectorCount)
	}
	if report.CeilingWarning != "" {
		t.Errorf("CeilingWarning: got %q, want empty", report.CeilingWarning)
	}
}

// TestCheckStorageBackend_Memory_Yellow verifies a yellow warning at 75k+1 vectors.
func TestCheckStorageBackend_Memory_Yellow(t *testing.T) {
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: memvec.YellowThreshold + 1,
	})

	if report.CeilingWarning != "yellow" {
		t.Errorf("CeilingWarning at yellow threshold+1: got %q, want %q", report.CeilingWarning, "yellow")
	}
}

// TestCheckStorageBackend_Memory_Red verifies a red warning at 90k+1 vectors.
func TestCheckStorageBackend_Memory_Red(t *testing.T) {
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: memvec.RedThreshold + 1,
	})

	if report.CeilingWarning != "red" {
		t.Errorf("CeilingWarning at red threshold+1: got %q, want %q", report.CeilingWarning, "red")
	}
}

// TestCheckStorageBackend_Memory_BelowYellow verifies no warning below the yellow threshold.
func TestCheckStorageBackend_Memory_BelowYellow(t *testing.T) {
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: memvec.YellowThreshold - 1,
	})
	if report.CeilingWarning != "" {
		t.Errorf("CeilingWarning below yellow: got %q, want empty", report.CeilingWarning)
	}
}

// TestCheckStorageBackend_Memory_AtRedThreshold verifies yellow (not red) at exactly the red threshold.
func TestCheckStorageBackend_Memory_AtRedThreshold(t *testing.T) {
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: memvec.RedThreshold,
	})
	if report.CeilingWarning != "yellow" {
		t.Errorf("CeilingWarning at exact red threshold: got %q, want %q", report.CeilingWarning, "yellow")
	}
}

// TestCheckStorageBackend_Usearch_NoCeiling verifies that usearch never triggers
// a ceiling warning (the threshold logic applies only to the memory backend).
func TestCheckStorageBackend_Usearch_NoCeiling(t *testing.T) {
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "usearch",
		VectorCount: 500_000,
	})
	if report.CeilingWarning != "" {
		t.Errorf("usearch: CeilingWarning: got %q, want empty (no ceiling for usearch)", report.CeilingWarning)
	}
}

// TestCheckStorageBackend_VectorCountPassthrough verifies VectorCount is echoed.
func TestCheckStorageBackend_VectorCountPassthrough(t *testing.T) {
	const want = 42
	report := doctor.CheckStorageBackend(doctor.StorageBackendParams{
		Backend:     "memory",
		VectorCount: want,
	})
	if report.VectorCount != want {
		t.Errorf("VectorCount: got %d, want %d", report.VectorCount, want)
	}
}
