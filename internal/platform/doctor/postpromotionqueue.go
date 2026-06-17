// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// ReviewFailureFindingID derives the branch-stable finding_id of the
// review-pipeline-failure companion finding for a failed review row anchored
// on the promotion commit gitSHA. It delegates to review.FailureFindingID so
// the probe, the review Handler (emit), and the close path all agree on the
// same finding identity.
func ReviewFailureFindingID(repoID, branch, gitSHA string) string {
	return review.FailureFindingID(repoID, branch, gitSHA)
}

// QueueCount holds the row count for a single state×work_kind combination.
type QueueCount struct {
	State    string `json:"state"`
	WorkKind string `json:"work_kind"`
	Count    int    `json:"count"`
}

// FailedRow describes a single permanently-failed post-promotion-queue row.
type FailedRow struct {
	Seq      int64  `json:"seq"`
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
	GitSHA   string `json:"git_sha"`
	WorkKind string `json:"work_kind"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	// MissingFinding is true for a failed review row that has no open
	// review-pipeline-failure companion finding - a broken-invariant state:
	// the failure has silently vanished instead of parking as a finding.
	MissingFinding bool `json:"missing_finding,omitempty"`
}

// PostPromotionQueueReport is the result of CheckPostPromotionQueue.
//
//	Status "healthy" - no failed rows
//	Status "degraded" - at least one failed row exists (review rows in this
//	  state must carry an open companion finding - the designed sticky state)
//	Status "broken" - DB could not be opened/pinged, OR a failed review
//	  row has no open review-pipeline-failure companion finding
type PostPromotionQueueReport struct {
	Counts     []QueueCount  `json:"counts"`
	FailedRows []FailedRow   `json:"failed_rows"`
	Status     health.Status `json:"status"`
	// OrphanCount is the number of failed rows whose repo_id is no longer
	// registered - the exact set `--purge-orphans` would clear. Surfaced so
	// the textual probe can point operators at the remediation.
	OrphanCount int `json:"orphan_count,omitempty"`
}

// CheckPostPromotionQueue opens the SQLite DB at dbPath read-only and
// returns counts grouped by state×work_kind plus any permanently-failed rows.
// If the DB cannot be opened or reached, it returns Status "broken" with a nil error.
func CheckPostPromotionQueue(dbPath string) (PostPromotionQueueReport, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=1000", dbPath)
	db, err := sql.Open(sqldriver.Name, dsn)
	if err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}

	counts, err := queryQueueCounts(db)
	if err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}

	failedRows, err := queryFailedRows(db)
	if err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}

	// Review-failure invariant ( AC3): every failed review row must
	// have an open review-pipeline-failure companion finding. A failed review
	// row WITHOUT one means the failure vanished silently - that is broken.
	anyMissingFinding, err := markMissingReviewFindings(db, failedRows)
	if err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}

	// Escalate from the best state to the worst observed condition, using
	// health.WorseThan as the single precedence authority (healthy <
	// degraded < broken). Failed rows are the designed sticky-degraded
	// state; a failed review row missing its companion finding is broken.
	status := health.StatusHealthy
	if len(failedRows) > 0 && health.StatusDegraded.WorseThan(status) {
		status = health.StatusDegraded
	}
	if anyMissingFinding && health.StatusBroken.WorseThan(status) {
		status = health.StatusBroken
	}

	orphans, err := queryOrphanCount(db)
	if err != nil {
		return PostPromotionQueueReport{Status: health.StatusBroken}, nil
	}

	return PostPromotionQueueReport{
		Counts:      counts,
		FailedRows:  failedRows,
		Status:      status,
		OrphanCount: orphans,
	}, nil
}

// markMissingReviewFindings checks, for each failed review row, whether an open
// review-pipeline-failure companion finding exists. It sets FailedRow.MissingFinding
// in place and reports whether any row is missing its finding.
func markMissingReviewFindings(db *sql.DB, failedRows []FailedRow) (bool, error) {
	any := false
	for i := range failedRows {
		fr := &failedRows[i]
		if fr.WorkKind != "review" {
			continue
		}
		findingID := ReviewFailureFindingID(fr.RepoID, fr.Branch, fr.GitSHA)
		var open int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM findings
			  WHERE finding_id = ? AND branch = ? AND rule = 'review-pipeline-failure' AND state = 'open'`,
			findingID, fr.Branch,
		).Scan(&open)
		if err != nil {
			return false, err
		}
		if open == 0 {
			fr.MissingFinding = true
			any = true
		}
	}
	return any, nil
}

func queryQueueCounts(db *sql.DB) ([]QueueCount, error) {
	rows, err := db.Query(
		`SELECT state, work_kind, COUNT(*) FROM post_promotion_queue GROUP BY state, work_kind`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []QueueCount
	for rows.Next() {
		var c QueueCount
		if err := rows.Scan(&c.State, &c.WorkKind, &c.Count); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// PurgeOrphanFailedRows deletes failed post-promotion-queue rows whose
// repo_id is no longer present in the repos table - the only case where a
// failed row can never make progress because the repo it targets has been
// deregistered. Returns the number of rows deleted.
// without this, removing a repo via `veska repo remove` leaves
// its failed rows behind, permanently dragging the
// doctor-status rollup to "degraded".
// Opens the DB read/write (not the read-only DSN CheckPostPromotionQueue
// uses) so the DELETE can run. Safe to call when no orphans exist (returns 0).
func PurgeOrphanFailedRows(dbPath string) (int64, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000", dbPath)
	db, err := sql.Open(sqldriver.Name, dsn)
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return 0, fmt.Errorf("ping db: %w", err)
	}
	res, err := db.Exec(
		`DELETE FROM post_promotion_queue
		  WHERE state = 'failed'
		    AND repo_id NOT IN (SELECT repo_id FROM repos)`,
	)
	if err != nil {
		return 0, fmt.Errorf("delete orphans: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// queryOrphanCount returns the count of failed rows whose repo_id is no
// longer in the repos table - the set --purge-orphans would clear
// A DB that doesn't carry the repos table (e.g. some test
// fixtures) yields 0 rather than an error, since the orphan concept
// requires the repos table to be meaningful.
func queryOrphanCount(db *sql.DB) (int, error) {
	var present int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='repos'`,
	).Scan(&present); err != nil || present == 0 {
		return 0, nil
	}
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM post_promotion_queue
		  WHERE state = 'failed'
		    AND repo_id NOT IN (SELECT repo_id FROM repos)`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count orphans: %w", err)
	}
	return n, nil
}

func queryFailedRows(db *sql.DB) ([]FailedRow, error) {
	rows, err := db.Query(
		`SELECT seq, repo_id, branch, git_sha, work_kind, attempts, COALESCE(error, '')
		 FROM post_promotion_queue WHERE state = 'failed'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var failed []FailedRow
	for rows.Next() {
		var f FailedRow
		if err := rows.Scan(&f.Seq, &f.RepoID, &f.Branch, &f.GitSHA, &f.WorkKind, &f.Attempts, &f.Error); err != nil {
			return nil, err
		}
		failed = append(failed, f)
	}
	return failed, rows.Err()
}
