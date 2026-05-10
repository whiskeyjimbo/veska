// Package gcsweep benchmarks branch-GC sweep: deletes branches from SQLite,
// records wall-clock time, disk size before/after, and WAL size before/after.
package gcsweep

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// GCSweepResult holds all metrics captured during a GC sweep.
type GCSweepResult struct {
	BranchesBefore  int   `json:"branches_before"`
	BranchesAfter   int   `json:"branches_after"`
	BranchesDeleted int   `json:"branches_deleted"`
	WallMs          int64 `json:"wall_ms"`
	DiskBeforeBytes int64 `json:"disk_before_bytes"`
	DiskAfterBytes  int64 `json:"disk_after_bytes"`
	WALBeforeBytes  int64 `json:"wal_before_bytes"`
	WALAfterBytes   int64 `json:"wal_after_bytes"`
	// ReclaimBytes is DiskBeforeBytes - DiskAfterBytes (can be 0 if WAL not fully flushed).
	ReclaimBytes int64 `json:"reclaim_bytes"`
}

// CountBranches returns the number of distinct branch values in the nodes table.
func CountBranches(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT branch) FROM nodes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count branches: %w", err)
	}
	return n, nil
}

// fileSize returns the size of a file in bytes, or 0 if the file doesn't exist.
func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// RunGCSweep deletes branches not in keepBranches from db and records metrics.
//
// Steps:
//  1. Ensure PRAGMA foreign_keys=ON
//  2. Measure disk + WAL sizes before
//  3. DELETE FROM nodes WHERE branch NOT IN (keepBranches...) — edges+findings cascade
//  4. PRAGMA wal_checkpoint(TRUNCATE)
//  5. VACUUM (to reclaim space after bulk delete)
//  6. Measure disk + WAL sizes after
//  7. Record wall-clock
//
// For in-memory DB (dbPath=""), disk sizes will be 0 — that is expected.
func RunGCSweep(db *sql.DB, dbPath string, keepBranches []string) (GCSweepResult, error) {
	var result GCSweepResult

	// Step 1: ensure FK enforcement.
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return result, fmt.Errorf("pragma foreign_keys: %w", err)
	}

	// Count branches before.
	before, err := CountBranches(db)
	if err != nil {
		return result, err
	}
	result.BranchesBefore = before

	// Step 2: measure disk + WAL sizes before.
	walPath := dbPath + "-wal"
	result.DiskBeforeBytes = fileSize(dbPath)
	result.WALBeforeBytes = fileSize(walPath)

	// Step 3: build DELETE statement with NOT IN clause.
	start := time.Now()

	if len(keepBranches) == 0 {
		// Delete all branches.
		if _, err := db.Exec(`DELETE FROM nodes`); err != nil {
			return result, fmt.Errorf("delete all nodes: %w", err)
		}
	} else {
		placeholders := make([]string, len(keepBranches))
		args := make([]any, len(keepBranches))
		for i, b := range keepBranches {
			placeholders[i] = "?"
			args[i] = b
		}
		query := fmt.Sprintf(
			`DELETE FROM nodes WHERE branch NOT IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := db.Exec(query, args...); err != nil {
			return result, fmt.Errorf("delete nodes: %w", err)
		}
	}

	// Step 4: WAL checkpoint to flush WAL to main DB file.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return result, fmt.Errorf("wal_checkpoint: %w", err)
	}

	// Step 5: VACUUM to reclaim space.
	if _, err := db.Exec(`VACUUM`); err != nil {
		return result, fmt.Errorf("vacuum: %w", err)
	}

	result.WallMs = time.Since(start).Milliseconds()

	// Step 6: measure disk + WAL sizes after.
	result.DiskAfterBytes = fileSize(dbPath)
	result.WALAfterBytes = fileSize(walPath)
	result.ReclaimBytes = result.DiskBeforeBytes - result.DiskAfterBytes

	// Count branches after.
	after, err := CountBranches(db)
	if err != nil {
		return result, err
	}
	result.BranchesAfter = after
	result.BranchesDeleted = result.BranchesBefore - result.BranchesAfter

	return result, nil
}
