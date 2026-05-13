package doctor

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

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
	WorkKind string `json:"work_kind"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

// PostPromotionQueueReport is the result of CheckPostPromotionQueue.
//
//   - Status "healthy"  — no failed rows
//   - Status "degraded" — at least one failed row exists
//   - Status "broken"   — DB could not be opened or pinged
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

	status := "healthy"
	if len(failedRows) > 0 {
		status = "degraded"
	}

	return PostPromotionQueueReport{
		Counts:     counts,
		FailedRows: failedRows,
		Status:     status,
	}, nil
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
		`SELECT seq, repo_id, branch, work_kind, attempts, COALESCE(error, '')
		 FROM post_promotion_queue WHERE state = 'failed'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var failed []FailedRow
	for rows.Next() {
		var f FailedRow
		if err := rows.Scan(&f.Seq, &f.RepoID, &f.Branch, &f.WorkKind, &f.Attempts, &f.Error); err != nil {
			return nil, err
		}
		failed = append(failed, f)
	}
	return failed, rows.Err()
}
