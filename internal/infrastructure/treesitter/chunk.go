package treesitter

import (
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// chunkLineWindow is the size of one chunk in lines. 80 is a
// compromise: small enough that a single chunk's embedding stays
// semantically focused (a long function body across multiple chunks
// still gets distinct embeddings per section), large enough that the
// chunk count per file stays bounded and the embedding cost is
// proportionate to file size, not symbol count. Semble uses a similar
// window.
const chunkLineWindow = 80

// chunkFile walks src in chunkLineWindow-sized line windows and emits
// one KindChunk node per window whose line range is NOT fully covered
// by an existing symbol. Symbols already produce per-symbol embeddings;
// chunks fill in everything between them — package vars, init guts,
// top-of-file commentary, helper TS modules without classes — so
// semantic search can find non-declaration code.
// IDs are deterministic per (repoID, path, start, end) so promotion is
// idempotent. raw_content is populated so the embedder + FTS index
// pick the chunk up through the same pipeline as symbol nodes.
func chunkFile(repoID, path string, src []byte, symbols []*domain.Node) []*domain.Node {
	if len(src) == 0 {
		return nil
	}

	lineStarts := bytesToLineOffsets(src)
	totalLines := len(lineStarts)
	if totalLines == 0 {
		return nil
	}

	// Walk the file's [1, totalLines] range in left-to-right order,
	// emitting chunks for each maximal gap between symbol ranges. A
	// chunk never overlaps a symbol: symbols are already indexed as
	// their own nodes, so re-embedding their body inside a chunk would
	// just duplicate retrieval candidates.
	uncovered := uncoveredRanges(symbols, totalLines)

	var chunks []*domain.Node
	for _, r := range uncovered {
		for start := r.Start; start <= r.End; start += chunkLineWindow {
			end := min(start+chunkLineWindow-1, r.End)
			startByte := lineStarts[start-1]
			var endByte int
			if end < totalLines {
				endByte = lineStarts[end] // start of next line, exclusive
			} else {
				endByte = len(src)
			}
			body := string(src[startByte:endByte])
			// Skip whitespace-only windows (blank-line gaps between
			// symbols). They embed to near-anything and pollute search
			// results, ranking above real code.
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

// uncoveredRanges returns the maximal [start,end] line intervals within
// [1, totalLines] that are NOT covered by any symbol's Lines range.
// Used by chunkFile to emit chunks only for non-declaration code.
func uncoveredRanges(symbols []*domain.Node, totalLines int) []domain.LineRange {
	covered := make([]bool, totalLines+2) // sentinel slots for boundary loop
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

// bytesToLineOffsets returns a slice where index i is the byte offset
// of the start of line (i+1). Used to convert line-window boundaries
// back to a byte slice for raw_content. A trailing newline contributes
// no extra entry; the last line is the one containing the final byte.
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
