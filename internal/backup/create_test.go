package backup_test

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/backup"
)

// seedDB creates a minimal SQLite database at dbPath.
func seedDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes VALUES ('n1','hello')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// tarEntries returns the set of file names inside a .tar.gz archive.
func tarEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	entries := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		entries[hdr.Name] = true
	}
	return entries
}

func TestCreateBackupBasic(t *testing.T) {
	veskaHome := t.TempDir()
	backupDir := t.TempDir()

	dbPath := filepath.Join(veskaHome, "veska.db")
	seedDB(t, dbPath)

	result, err := backup.Create(backup.CreateOptions{
		DBPath:     dbPath,
		EngramHome: veskaHome,
		BackupDir:  backupDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Tarball must exist.
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("tarball missing: %v", err)
	}

	// Must be a valid gzip.
	if err := backup.VerifyGzip(result.Path); err != nil {
		t.Fatalf("VerifyGzip: %v", err)
	}

	// Must contain manifest.json.
	entries := tarEntries(t, result.Path)
	if !entries["manifest.json"] {
		t.Fatalf("tarball missing manifest.json; got %v", entries)
	}
}

func TestCreateBackupCopiesAuditLog(t *testing.T) {
	veskaHome := t.TempDir()
	backupDir := t.TempDir()

	dbPath := filepath.Join(veskaHome, "veska.db")
	seedDB(t, dbPath)

	// Seed audit.jsonl.
	auditPath := filepath.Join(veskaHome, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(`{"event":"test"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write audit.jsonl: %v", err)
	}

	result, err := backup.Create(backup.CreateOptions{
		DBPath:     dbPath,
		EngramHome: veskaHome,
		BackupDir:  backupDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries := tarEntries(t, result.Path)
	if !entries["audit.jsonl"] {
		t.Fatalf("tarball missing audit.jsonl; got %v", entries)
	}
}

func TestCreateBackupSkipsMissingConfig(t *testing.T) {
	veskaHome := t.TempDir()
	backupDir := t.TempDir()

	dbPath := filepath.Join(veskaHome, "veska.db")
	seedDB(t, dbPath)

	// Deliberately do NOT create config.toml.
	_, err := backup.Create(backup.CreateOptions{
		DBPath:     dbPath,
		EngramHome: veskaHome,
		BackupDir:  backupDir,
	})
	if err != nil {
		t.Fatalf("Create should succeed without config.toml: %v", err)
	}
}

func TestCreateBackupResult(t *testing.T) {
	veskaHome := t.TempDir()
	backupDir := t.TempDir()

	dbPath := filepath.Join(veskaHome, "veska.db")
	seedDB(t, dbPath)

	result, err := backup.Create(backup.CreateOptions{
		DBPath:     dbPath,
		EngramHome: veskaHome,
		BackupDir:  backupDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.Path == "" {
		t.Fatal("CreateResult.Path is empty")
	}
	if result.SizeBytes <= 0 {
		t.Fatalf("CreateResult.SizeBytes=%d, want >0", result.SizeBytes)
	}
}

func TestCreateBackupManifestFields(t *testing.T) {
	veskaHome := t.TempDir()
	backupDir := t.TempDir()

	dbPath := filepath.Join(veskaHome, "veska.db")
	seedDB(t, dbPath)

	result, err := backup.Create(backup.CreateOptions{
		DBPath:     dbPath,
		EngramHome: veskaHome,
		BackupDir:  backupDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Open tarball and extract manifest.json.
	f, err := os.Open(result.Path)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		if m["created_at"] == "" {
			t.Error("manifest missing created_at")
		}
		if m["veska_home"] == "" {
			t.Error("manifest missing veska_home")
		}
		if m["go_version"] == "" {
			t.Error("manifest missing go_version")
		}
		return
	}
	t.Fatal("manifest.json not found in tarball")
}
