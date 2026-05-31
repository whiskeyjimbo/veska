package doctor

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// BackupReport holds the result of a backup directory integrity check,
// matching the SOLO-13 §2.1 data schema for the backup subsystem.
type BackupReport struct {
	BackupDir   string        `json:"backup_dir"`
	LatestFile  string        `json:"latest_file"`
	LatestAge   time.Duration `json:"latest_age_ns"`
	AgeHours    float64       `json:"age_hours"`
	FileCount   int           `json:"file_count"`
	Status      health.Status `json:"status"`
	VerifyError string        `json:"verify_error,omitempty"`
}

// CheckBackup scans backupDir for *.tar.gz files, selects the most recently
// modified one, reports its age, and verifies that it is a valid gzip archive.
//
// Status values:
//   - "healthy"  — at least one backup exists and its gzip header is valid
//   - "degraded" — no *.tar.gz files found in backupDir
//   - "broken"   — most recent backup exists but fails gzip verification
func CheckBackup(backupDir string) (BackupReport, error) {
	matches, err := filepath.Glob(filepath.Join(backupDir, "*.tar.gz"))
	if err != nil {
		return BackupReport{}, err
	}

	report := BackupReport{
		BackupDir: backupDir,
		FileCount: len(matches),
	}

	if len(matches) == 0 {
		report.Status = health.StatusDegraded
		return report, nil
	}

	// Find the most recently modified file.
	var latestPath string
	var latestMod time.Time
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if latestPath == "" || info.ModTime().After(latestMod) {
			latestPath = p
			latestMod = info.ModTime()
		}
	}

	age := time.Since(latestMod)
	report.LatestFile = latestPath
	report.LatestAge = age
	report.AgeHours = age.Hours()

	// Verify gzip header by opening and reading at least the first byte.
	if verifyErr := verifyGzip(latestPath); verifyErr != nil {
		report.Status = health.StatusBroken
		report.VerifyError = verifyErr.Error()
		return report, nil
	}

	report.Status = health.StatusHealthy
	return report, nil
}

// verifyGzip opens path as a gzip stream and reads the header.
// Returns nil on success, or an error describing the failure.
func verifyGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	// Read at least one byte to confirm the stream is readable.
	buf := make([]byte, 1)
	_, err = gr.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}
