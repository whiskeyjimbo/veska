// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package fatfiles implements a per-file line-count ratchet.
// Unlike `make lint-size` (which enforces per-FUNCTION limits on CHANGED code
// only via golangci-lint --new-from-merge-base), this ratchet tracks the TOTAL
// line count of a checked-in inventory of already-oversized files. The recorded
// value is a ceiling that can only go DOWN: any inventoried file that grows past
// its recorded line count fails the gate, forcing the backlog to shrink over
// time instead of being grandfathered forever.
// The inventory file (inventory.txt) is a list of `path<space>maxLOC` records,
// one per line; blank lines and `#`-prefixed comments are ignored.
package fatfiles

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Entry is one inventoried file and its recorded maximum line count.
type Entry struct {
	Path   string
	MaxLOC int
}

// Violation describes a file that breaches the ratchet, in one of two ways:
// either it GREW past its recorded ceiling (Grew == true), or it is listed in
// the inventory but no longer exists on disk (Missing == true).
type Violation struct {
	Path        string
	RecordedLOC int
	CurrentLOC  int
	Missing     bool
}

func (v Violation) String() string {
	if v.Missing {
		return fmt.Sprintf("FATFILE STALE: %s is inventoried (max %d) but does not exist; remove its entry", v.Path, v.RecordedLOC)
	}
	return fmt.Sprintf("FATFILE GREW: %s is %d LOC, exceeds recorded ceiling of %d (lower the file or it must shrink)", v.Path, v.CurrentLOC, v.RecordedLOC)
}

// Check compares the recorded inventory against the current sizes and returns
// every violation. A file is a violation when its current size exceeds the
// recorded ceiling, or when it is inventoried but absent from currentSizes.
// Files at or below their recorded ceiling are fine (the ratchet only ratchets
// down). currentSizes maps file path -> current LOC.
func Check(inventory []Entry, currentSizes map[string]int) []Violation {
	var violations []Violation
	for _, e := range inventory {
		cur, ok := currentSizes[e.Path]
		if !ok {
			violations = append(violations, Violation{
				Path: e.Path, RecordedLOC: e.MaxLOC, Missing: true,
			})
			continue
		}
		if cur > e.MaxLOC {
			violations = append(violations, Violation{
				Path: e.Path, RecordedLOC: e.MaxLOC, CurrentLOC: cur,
			})
		}
	}
	return violations
}

// ParseInventory reads `path maxLOC` records, skipping blank/comment lines.
func ParseInventory(r io.Reader) ([]Entry, error) {
	var entries []Entry
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("inventory line %d: want `path maxLOC`, got %q", line, text)
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("inventory line %d: bad LOC %q: %w", line, fields[1], err)
		}
		entries = append(entries, Entry{Path: fields[0], MaxLOC: n})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// CountLOC returns the physical line count of the file at path, matching `wc -l`
// semantics (number of newline-terminated lines, plus one if the final line is
// not newline-terminated).
func CountLOC(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var lines int
	var trailing bool
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' {
				lines++
				trailing = false
			} else {
				trailing = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if trailing {
		lines++
	}
	return lines, nil
}

// CurrentSizes counts the LOC of every inventoried path, relative to root.
func CurrentSizes(root string, inventory []Entry) (map[string]int, error) {
	sizes := make(map[string]int, len(inventory))
	for _, e := range inventory {
		n, err := CountLOC(filepath.Join(root, e.Path))
		if err != nil {
			if os.IsNotExist(err) {
				continue // surfaced as a Missing violation by Check
			}
			return nil, err
		}
		sizes[e.Path] = n
	}
	return sizes, nil
}

// CheckDir loads the inventory at inventoryPath, measures the current sizes of
// each entry relative to root, and returns all violations.
func CheckDir(root, inventoryPath string) ([]Violation, error) {
	f, err := os.Open(inventoryPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	inv, err := ParseInventory(f)
	if err != nil {
		return nil, err
	}
	sizes, err := CurrentSizes(root, inv)
	if err != nil {
		return nil, err
	}
	v := Check(inv, sizes)
	sort.Slice(v, func(i, j int) bool { return v[i].Path < v[j].Path })
	return v, nil
}
