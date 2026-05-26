package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// VerifyResult summarises the outcome of a backup integrity check.
type VerifyResult struct {
	// DBIntegrityOK is true if PRAGMA integrity_check returned "ok".
	DBIntegrityOK bool
	// ForeignKeyOK is true if PRAGMA foreign_key_check returned no rows.
	ForeignKeyOK bool
	// AuditPresent is true if audit.jsonl was present in the tarball.
	AuditPresent bool
	// AuditJSONLOK is true if every line in audit.jsonl parsed as valid JSON.
	// Always false when AuditPresent is false.
	AuditJSONLOK bool
	// Status is one of "healthy", "degraded", or "broken".
	//   healthy  — all present checks passed
	//   degraded — audit.jsonl present but malformed; DB checks passed
	//   broken   — veska.db could not be extracted or failed integrity checks
	Status string
}

// Verify extracts veska.db (and optionally audit.jsonl) from the .tar.gz at
// path, runs PRAGMA integrity_check and PRAGMA foreign_key_check on the
// database, and validates each line of audit.jsonl as JSON.
//
// Exit codes (via caller): 0=healthy, 1=degraded, 2=broken.
func Verify(path string) (VerifyResult, error) {
	// Extract files into a temp dir.
	tmpDir, err := os.MkdirTemp("", "veska-verify-*")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("backup verify: MkdirTemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dbBytes, auditBytes, auditPresent, err := extractVerifyFiles(path)
	if err != nil {
		return VerifyResult{Status: "broken"}, nil //nolint:nilerr // extraction failure → broken, not a caller error
	}

	// Write veska.db to temp dir.
	dbPath := filepath.Join(tmpDir, "veska.db")
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return VerifyResult{Status: "broken"}, fmt.Errorf("backup verify: write veska.db: %w", err)
	}

	// Run SQLite integrity checks.
	integrityOK, fkOK, err := checkSQLite(dbPath)
	if err != nil {
		// Could not even open the DB.
		return VerifyResult{DBIntegrityOK: false, ForeignKeyOK: false, Status: "broken"}, nil //nolint:nilerr
	}
	if !integrityOK || !fkOK {
		return VerifyResult{
			DBIntegrityOK: integrityOK,
			ForeignKeyOK:  fkOK,
			Status:        "broken",
		}, nil
	}

	result := VerifyResult{
		DBIntegrityOK: true,
		ForeignKeyOK:  true,
		AuditPresent:  auditPresent,
	}

	if auditPresent {
		result.AuditJSONLOK = checkJSONL(auditBytes)
		if !result.AuditJSONLOK {
			result.Status = "degraded"
			return result, nil
		}
	}

	result.Status = "healthy"
	return result, nil
}

// extractVerifyFiles reads the .tar.gz at path and returns the raw bytes for
// veska.db and audit.jsonl (if present).  Returns an error if veska.db is
// not found in the archive.
func extractVerifyFiles(path string) (dbBytes []byte, auditBytes []byte, auditPresent bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, false, fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	dbFound := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, false, fmt.Errorf("tar: %w", err)
		}

		switch hdr.Name {
		case "veska.db":
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, tr); err != nil {
				return nil, nil, false, fmt.Errorf("read veska.db: %w", err)
			}
			dbBytes = buf.Bytes()
			dbFound = true

		case "audit.jsonl":
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, tr); err != nil {
				return nil, nil, false, fmt.Errorf("read audit.jsonl: %w", err)
			}
			auditBytes = buf.Bytes()
			auditPresent = true
		}
	}

	if !dbFound {
		return nil, nil, false, fmt.Errorf("veska.db not found in archive")
	}
	return dbBytes, auditBytes, auditPresent, nil
}

// checkSQLite opens the SQLite database at dbPath and runs PRAGMA
// integrity_check and PRAGMA foreign_key_check.  Returns (integrityOK, fkOK,
// error).  An error is returned only if the database cannot be opened.
func checkSQLite(dbPath string) (integrityOK bool, fkOK bool, err error) {
	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		return false, false, err
	}
	defer db.Close()

	ctx := context.Background()

	// PRAGMA integrity_check: a healthy DB returns a single row with value "ok".
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return false, false, err
	}
	defer rows.Close()

	integrityOK = true
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			return false, false, err
		}
		if val != "ok" {
			integrityOK = false
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	rows.Close()

	// PRAGMA foreign_key_check: no rows means no violations.
	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return integrityOK, false, err
	}
	defer fkRows.Close()

	fkOK = !fkRows.Next() // if there are rows, there are violations
	if err := fkRows.Err(); err != nil {
		return integrityOK, false, err
	}

	return integrityOK, fkOK, nil
}

// checkJSONL returns true if every non-empty line in data is valid JSON.
func checkJSONL(data []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return false
		}
	}
	return true
}
