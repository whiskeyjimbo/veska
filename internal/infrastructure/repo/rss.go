// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

// DefaultRSSSoftCap defines the default global RSS soft cap (2 GiB).
const DefaultRSSSoftCap = int64(2 * 1024 * 1024 * 1024)

// rssFileCountCap defines the upper bound for the file count during RSS estimation.
const rssFileCountCap = int64(100_000)

// rssBytesPerFile is the estimated resident set size memory consumed per tracked file in bytes.
const rssBytesPerFile = int64(512)

// walkAndCountFiles counts non-directory files, structured as a package-level variable for test stubbing.
var walkAndCountFiles = func(root string) (int64, error) {
	var count int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip unreadable files or directories.
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

// ProjectRepoRSS estimates the resident set size memory overhead of tracking the repository at rootPath.
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

// CheckRSSBudget evaluates the memory budget, returning an error if total projected RSS exceeds the soft cap.
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
