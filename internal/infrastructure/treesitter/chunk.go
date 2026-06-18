// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// chunkLineWindow specifies the line window size for a single chunk. An 80-line window
// balances semantic focus for search embeddings with chunk count limits per file.
const chunkLineWindow = 80

// chunkFile divides the file into fixed-size line windows and emits a KindChunk node
// for gaps that are not covered by any symbol node. This ensures that non-declaration
// code (like init logic or module helpers) remains searchable.
func chunkFile(repoID, path string, src []byte, symbols []*domain.Node) []*domain.Node {
	if len(src) == 0 {
		return nil
	}

	lineStarts := bytesToLineOffsets(src)
	totalLines := len(lineStarts)
	if totalLines == 0 {
		return nil
	}

	// We emit chunks only for gaps between symbol ranges to prevent redundant indexing
	// of symbols that are already retrieved separately.
	uncovered := uncoveredRanges(symbols, totalLines)

	var chunks []*domain.Node
	for _, r := range uncovered {
		for start := r.Start; start <= r.End; start += chunkLineWindow {
			end := min(start+chunkLineWindow-1, r.End)
			startByte := lineStarts[start-1]
			var endByte int
			if end < totalLines {
				endByte = lineStarts[end]
			} else {
				endByte = len(src)
			}
			body := string(src[startByte:endByte])
			// Skip whitespace-only windows to avoid polluting search results with blank lines.
			if strings.TrimSpace(body) == "" {
				continue
			}
			name := fmt.Sprintf("chunk:%d-%d", start, end)
			id := nodeID(repoID, path, domain.KindChunk, name)
			n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindChunk}, domain.WithLines(domain.LineRange{Start: start, End: end}), domain.WithRawContent(body))
			if err != nil {
				continue
			}
			chunks = append(chunks, n)
		}
	}
	return chunks
}

// uncoveredRanges returns the maximal line intervals that are not covered by any symbol.
func uncoveredRanges(symbols []*domain.Node, totalLines int) []domain.LineRange {
	covered := make([]bool, totalLines+2)
	for _, s := range symbols {
		if s == nil || s.Lines == nil {
			continue
		}
		lo, hi := s.Lines.Start, s.Lines.End
		if lo < 1 {
			lo = 1
		}
		if hi > totalLines {
			hi = totalLines
		}
		for i := lo; i <= hi; i++ {
			covered[i] = true
		}
	}
	var out []domain.LineRange
	start := 0
	for i := 1; i <= totalLines+1; i++ {
		if !covered[i] && start == 0 && i <= totalLines {
			start = i
		}
		if (covered[i] || i > totalLines) && start != 0 {
			out = append(out, domain.LineRange{Start: start, End: i - 1})
			start = 0
		}
	}
	return out
}

// bytesToLineOffsets returns the byte offsets corresponding to the start of each line.
func bytesToLineOffsets(src []byte) []int {
	if len(src) == 0 {
		return nil
	}
	offsets := []int{0}
	for i, b := range src {
		if b == '\n' && i+1 < len(src) {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}
