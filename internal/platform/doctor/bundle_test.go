package doctor_test

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// openTarball opens a .tar.gz file and returns a map of path → contents for all entries.
func openTarball(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("openTarball: open %s: %v", path, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("openTarball: gzip reader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("openTarball: tar.Next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("openTarball: read %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = data
	}
	return files
}

// setupVeskaHome creates a minimal fake veska home directory with veska.db and audit.jsonl.
func setupVeskaHome(t *testing.T, auditContent string) string {
	t.Helper()
	dir := t.TempDir()

	// Create a minimal veska.db so CheckStorage sees it.
	if err := os.WriteFile(filepath.Join(dir, "veska.db"), []byte("SQLite format 3"), 0o644); err != nil {
		t.Fatalf("setupVeskaHome: write veska.db: %v", err)
	}

	// Write audit.jsonl.
	if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(auditContent), 0o644); err != nil {
		t.Fatalf("setupVeskaHome: write audit.jsonl: %v", err)
	}

	return dir
}

// TestCreateBundleContents verifies that the tarball contains all required entries.
func TestCreateBundleContents(t *testing.T) {
	veskaHome := setupVeskaHome(t, `{"op":"sync","repo_id":"test-repo"}`+"\n")
	outDir := t.TempDir()

	result, err := doctor.CreateBundle(doctor.BundleOptions{
		VeskaHome: veskaHome,
		OutputDir: outDir,
		OllamaURL: "http://localhost:11434",
		ModelName: "nomic-embed-text",
	})
	if err != nil {
		t.Fatalf("CreateBundle: unexpected error: %v", err)
	}

	if result.Path == "" {
		t.Fatal("CreateBundle: result.Path is empty")
	}
	if result.FileCount == 0 {
		t.Fatal("CreateBundle: result.FileCount is 0")
	}

	files := openTarball(t, result.Path)

	required := []string{
		"manifest.json",
		"doctor/storage.json",
		"doctor/embedder.json",
		"doctor/egress.json",
		"doctor/config.json",
		"doctor/service.json",
		"doctor/post_promotion_queue.json",
		"audit.tail",
	}
	for _, name := range required {
		if _, ok := files[name]; !ok {
			t.Errorf("tarball missing required entry: %s (found: %v)", name, keys(files))
		}
	}

	if result.FileCount != len(required) {
		t.Errorf("FileCount=%d, want %d", result.FileCount, len(required))
	}
}

// TestCreateBundleManifest verifies manifest.json contains expected fields.
func TestCreateBundleManifest(t *testing.T) {
	veskaHome := setupVeskaHome(t, "")
	outDir := t.TempDir()

	result, err := doctor.CreateBundle(doctor.BundleOptions{
		VeskaHome: veskaHome,
		OutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("CreateBundle: unexpected error: %v", err)
	}

	files := openTarball(t, result.Path)
	raw, ok := files["manifest.json"]
	if !ok {
		t.Fatal("manifest.json not found in tarball")
	}

	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json unmarshal: %v", err)
	}

	if m["go_version"] == "" {
		t.Error("manifest.go_version is empty")
	}

	createdAt := m["created_at"]
	if createdAt == "" {
		t.Fatal("manifest.created_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Errorf("manifest.created_at %q not RFC3339: %v", createdAt, err)
	}

	if m["veska_home"] == "" {
		t.Error("manifest.veska_home is empty")
	}

	if m["platform"] == "" {
		t.Error("manifest.platform is empty")
	}
}

// TestCreateBundleRedaction verifies that secrets in audit.jsonl are redacted in audit.tail.
func TestCreateBundleRedaction(t *testing.T) {
	secret := "sk-abc123"
	auditLine := `{"op":"sync","token":"` + secret + `"}` + "\n"
	veskaHome := setupVeskaHome(t, auditLine)
	outDir := t.TempDir()

	result, err := doctor.CreateBundle(doctor.BundleOptions{
		VeskaHome: veskaHome,
		OutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("CreateBundle: unexpected error: %v", err)
	}

	files := openTarball(t, result.Path)
	auditTail, ok := files["audit.tail"]
	if !ok {
		t.Fatal("audit.tail not found in tarball")
	}

	content := string(auditTail)
	if strings.Contains(content, secret) {
		t.Errorf("audit.tail contains unredacted secret %q", secret)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("audit.tail does not contain [REDACTED]; content: %q", content)
	}
}

// TestCreateBundleDefaultOutputDir verifies that CreateBundle uses os.TempDir() when OutputDir is empty.
func TestCreateBundleDefaultOutputDir(t *testing.T) {
	veskaHome := setupVeskaHome(t, "")

	result, err := doctor.CreateBundle(doctor.BundleOptions{
		VeskaHome: veskaHome,
		OutputDir: "", // should default to os.TempDir()
	})
	if err != nil {
		t.Fatalf("CreateBundle: unexpected error: %v", err)
	}

	if result.Path == "" {
		t.Fatal("result.Path is empty")
	}
	// Verify the file actually exists.
	if _, err := os.Stat(result.Path); err != nil {
		t.Errorf("tarball not found at %s: %v", result.Path, err)
	}

	// Clean up from os.TempDir().
	t.Cleanup(func() { os.Remove(result.Path) })
}

// keys returns the sorted keys of a map for error messages.
func keys(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
