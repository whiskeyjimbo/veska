package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectRepoRSSSmall(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		path := filepath.Join(dir, "file"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("create file: %v", err)
		}
	}

	got, err := ProjectRepoRSS(dir)
	if err != nil {
		t.Fatalf("ProjectRepoRSS: %v", err)
	}
	want := int64(10 * 512)
	if got != want {
		t.Errorf("ProjectRepoRSS = %d, want %d", got, want)
	}
}

func TestProjectRepoRSSCapped(t *testing.T) {
	// Override walkAndCountFiles to simulate a count of 200,000 files.
	orig := walkAndCountFiles
	t.Cleanup(func() { walkAndCountFiles = orig })
	walkAndCountFiles = func(_ string) (int64, error) {
		return 200_000, nil
	}

	got, err := ProjectRepoRSS(t.TempDir())
	if err != nil {
		t.Fatalf("ProjectRepoRSS: %v", err)
	}
	want := int64(100_000 * 512)
	if got != want {
		t.Errorf("ProjectRepoRSS = %d, want %d (cap not applied)", got, want)
	}
}

func TestCheckRSSBudgetOK(t *testing.T) {
	const GiB = int64(1024 * 1024 * 1024)
	err := CheckRSSBudget(1*GiB, 100*1024*1024, 2*GiB)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

func TestCheckRSSBudgetExceeded(t *testing.T) {
	const GiB = int64(1024 * 1024 * 1024)
	// Verify that a combined current and projected RSS (2.1 GiB) exceeding the cap (2 GiB) triggers an error.
	err := CheckRSSBudget(
		GiB+GiB*9/10, // ~1.9 GiB
		200*1024*1024,
		2*GiB,
	)
	if err == nil {
		t.Error("expected error when budget exceeded, got nil")
	}
}

func TestCurrentRSSReturnsNonNegative(t *testing.T) {
	rss, err := CurrentRSS()
	if err != nil {
		t.Fatalf("CurrentRSS: %v", err)
	}
	if rss < 0 {
		t.Errorf("CurrentRSS returned negative value: %d", rss)
	}
}
