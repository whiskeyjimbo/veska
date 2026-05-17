package doctor

import (
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/review"
	_ "modernc.org/sqlite"
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
	// review-pipeline-failure companion finding — a broken-invariant state:
	// the failure has silently vanished instead of parking as a finding.
	MissingFinding bool `json:"missing_finding,omitempty"`
}

// PostPromotionQueueReport is the result of CheckPostPromotionQueue.
//
//   - Status "healthy"  — no failed rows
//   - Status "degraded" — at least one failed row exists (review rows in this
//     state must carry an open companion finding — the designed sticky state)
//   - Status "broken"   — DB could not be opened/pinged, OR a failed review
//     row has no open review-pipeline-failure companion finding
type PostPromotionQueueReport struct {
	Counts     []QueueCount `json:"counts"`
	FailedRows []FailedRow  `json:"failed_rows"`
	Status     string       `json:"status"`
}

// CheckPostPromotionQueue opens the SQLite DB at dbPath read-only and
// returns counts grouped by state×work_kind plus any permanently-failed rows.
// If the DB cannot be opened or reached, it returns Status "broken" with a nil error.
func CheckPostPromotionQueue(dbPath string) (PostPromotionQueueReport, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=1000", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return PostPromotionQueueReport{Status: "broken"}, nil
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return PostPromotionQueueReport{Status: "broken"}, nil
	}

	counts, err := queryQueueCounts(db)
	if err != nil {
		return PostPromotionQueueReport{Status: "broken"}, nil
	}

	failedRows, err := queryFailedRows(db)
	if err != nil {
		return PostPromotionQueueReport{Status: "broken"}, nil
	}

	// Review-failure invariant (solov2-nz2.3 AC3): every failed review row must
	// have an open review-pipeline-failure companion finding. A failed review
	// row WITHOUT one means the failure vanished silently — that is broken.
	anyMissingFinding, err := markMissingReviewFindings(db, failedRows)
	if err != nil {
		return PostPromotionQueueReport{Status: "broken"}, nil
	}

	status := "healthy"
	if len(failedRows) > 0 {
		status = "degraded"
	}
	if anyMissingFinding {
		status = "broken"
	}

	return PostPromotionQueueReport{
		Counts:     counts,
		FailedRows: failedRows,
		Status:     status,
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
