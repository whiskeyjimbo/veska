package backup_test

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/engram/solov2/internal/backup"
)

// buildTarball writes a .tar.gz to tarPath containing the files described by
// entries (name -> content).  If content is nil the file is empty.
func buildTarball(t *testing.T, tarPath string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("create tarball: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	for name, content := range entries {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %s: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar Write %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
}

// minimalSQLiteDB creates a minimal valid SQLite database at dbPath and returns
// its bytes for embedding in a tarball.
func minimalSQLiteDB(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "engram.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE _health (ok INTEGER)`); err != nil {
		db.Close()
		t.Fatalf("create table: %v", err)
	}
	db.Close()

	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	return data
}

// TestVerifyHealthy builds a tarball with a valid engram.db (and valid
// audit.jsonl) and expects Status="healthy", all checks pass.
func TestVerifyHealthy(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "backup.tar.gz")

	dbBytes := minimalSQLiteDB(t)
	auditBytes := []byte(`{"event":"boot"}` + "\n" + `{"event":"shutdown"}` + "\n")

	buildTarball(t, tarPath, map[string][]byte{
		"engram.db":   dbBytes,
		"audit.jsonl": auditBytes,
	})

	result, err := backup.Verify(tarPath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Status != "healthy" {
		t.Errorf("Status=%q, want healthy", result.Status)
	}
	if !result.DBIntegrityOK {
		t.Error("DBIntegrityOK=false, want true")
	}
	if !result.ForeignKeyOK {
		t.Error("ForeignKeyOK=false, want true")
	}
	if !result.AuditPresent {
		t.Error("AuditPresent=false, want true")
	}
	if !result.AuditJSONLOK {
		t.Error("AuditJSONLOK=false, want true")
	}
}

// TestVerifyBrokenDB builds a tarball containing random bytes as engram.db.
// Verify should return Status="broken".
func TestVerifyBrokenDB(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "backup.tar.gz")

	buildTarball(t, tarPath, map[string][]byte{
		"engram.db": []byte("this is not a valid sqlite database"),
	})

	result, err := backup.Verify(tarPath)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if result.Status != "broken" {
		t.Errorf("Status=%q, want broken", result.Status)
	}
	if result.DBIntegrityOK {
		t.Error("DBIntegrityOK=true, want false for corrupt DB")
	}
}

// TestVerifyAuditJSONLMalformed builds a tarball with a valid engram.db but
// an audit.jsonl that has a non-JSON line.  Expects Status="degraded".
func TestVerifyAuditJSONLMalformed(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "backup.tar.gz")

	dbBytes := minimalSQLiteDB(t)
	// Line 2 is not valid JSON.
	malformedAudit := []byte(`{"event":"ok"}` + "\n" + "this is not json\n")

	buildTarball(t, tarPath, map[string][]byte{
		"engram.db":   dbBytes,
		"audit.jsonl": malformedAudit,
	})

	result, err := backup.Verify(tarPath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Status != "degraded" {
		t.Errorf("Status=%q, want degraded", result.Status)
	}
	if !result.DBIntegrityOK {
		t.Error("DBIntegrityOK=false, want true (DB is ok)")
	}
	if result.AuditJSONLOK {
		t.Error("AuditJSONLOK=true, want false for malformed audit")
	}
	if !result.AuditPresent {
		t.Error("AuditPresent=false, want true")
	}
}

// TestVerifyNoAuditJSONL builds a tarball that only contains engram.db.
// AuditPresent should be false and Status should be "healthy".
func TestVerifyNoAuditJSONL(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "backup.tar.gz")

	dbBytes := minimalSQLiteDB(t)

	buildTarball(t, tarPath, map[string][]byte{
		"engram.db": dbBytes,
	})

	result, err := backup.Verify(tarPath)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.AuditPresent {
		t.Error("AuditPresent=true, want false (no audit.jsonl in tarball)")
	}
	if result.Status != "healthy" {
		t.Errorf("Status=%q, want healthy", result.Status)
	}
	if !result.DBIntegrityOK {
		t.Error("DBIntegrityOK=false, want true")
	}
}
