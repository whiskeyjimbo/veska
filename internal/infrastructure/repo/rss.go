package repo

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

// DefaultRSSSoftCap is the global RSS soft cap: 2 GiB.
const DefaultRSSSoftCap = int64(2 * 1024 * 1024 * 1024)

// rssFileCountCap is the maximum number of files counted when projecting repo RSS.
const rssFileCountCap = int64(100_000)

// rssBytesPerFile is the estimated steady-state RSS contribution per tracked file.
const rssBytesPerFile = int64(512)

// walkAndCountFiles is a package-level var so tests can override it.
var walkAndCountFiles = func(root string) (int64, error) {
	var count int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

// ProjectRepoRSS estimates the steady-state RSS contribution of adding a repo
// rooted at rootPath. It counts files (capped at 100k) and multiplies by 512 bytes.
func ProjectRepoRSS(rootPath string) (int64, error) {
	count, err := walkAndCountFiles(rootPath)
	if err != nil {
		return 0, fmt.Errorf("project repo RSS: walk %s: %w", rootPath, err)
	}
	if count > rssFileCountCap {
		count = rssFileCountCap
	}
	return count * rssBytesPerFile, nil
}

// CheckRSSBudget returns an error if currentRSS + projectedRSS exceeds softCapBytes.
func CheckRSSBudget(currentRSS, projectedRSS, softCapBytes int64) error {
	total := currentRSS + projectedRSS
	if total > softCapBytes {
		return fmt.Errorf(
			"RSS budget exceeded: current=%d bytes, projected=%d bytes, total=%d bytes, cap=%d bytes; "+
				"reduce tracked repositories or raise the soft cap",
			currentRSS, projectedRSS, total, softCapBytes,
		)
	}
	return nil
}
