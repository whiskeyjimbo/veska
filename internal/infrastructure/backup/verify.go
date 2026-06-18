// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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

// VerifyResult summarizes the outcome of a backup integrity check.
type VerifyResult struct {
	// DBIntegrityOK indicates whether the SQLite database passed PRAGMA integrity_check.
	DBIntegrityOK bool
	// ForeignKeyOK indicates whether the SQLite database passed PRAGMA foreign_key_check without violations.
	ForeignKeyOK bool
	// AuditPresent indicates whether the audit log file was found in the archive.
	AuditPresent bool
	// AuditJSONLOK indicates whether all lines in the audit log are valid JSON.
	AuditJSONLOK bool
	// Status represents the health status of the backup ("healthy", "degraded", or "broken").
	// A healthy status indicates all database and audit log checks passed.
	// A degraded status indicates database checks passed but the audit log is malformed.
	// A broken status indicates the database could not be extracted or failed integrity checks.
	Status string
}

// Verify inspects the backup archive at the given path by checking database integrity,
// verifying foreign keys, and validating the structure of the audit log if present.
func Verify(path string) (VerifyResult, error) {
	tmpDir, err := os.MkdirTemp("", "veska-verify-*")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("backup verify: MkdirTemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dbBytes, auditBytes, auditPresent, err := extractVerifyFiles(path)
	if err != nil {
		return VerifyResult{Status: "broken"}, nil //nolint:nilerr // extraction failure → broken, not a caller error
	}

	dbPath := filepath.Join(tmpDir, "veska.db")
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return VerifyResult{Status: "broken"}, fmt.Errorf("backup verify: write veska.db: %w", err)
	}

	integrityOK, fkOK, err := checkSQLite(dbPath)
	if err != nil {
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

func checkSQLite(dbPath string) (integrityOK bool, fkOK bool, err error) {
	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		return false, false, err
	}
	defer db.Close()

	ctx := context.Background()

	// A healthy database returns a single row with the value "ok" from PRAGMA integrity_check.
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

	// PRAGMA foreign_key_check returns no rows if there are no foreign key violations.
	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return integrityOK, false, err
	}
	defer fkRows.Close()

	fkOK = !fkRows.Next()
	if err := fkRows.Err(); err != nil {
		return integrityOK, false, err
	}

	return integrityOK, fkOK, nil
}

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
