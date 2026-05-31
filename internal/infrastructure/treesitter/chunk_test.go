package treesitter

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestChunkFile_EmitsChunksForUncoveredRanges: a file with one symbol
// at lines 10-15 should produce chunks for the surrounding regions,
// not for the symbol's range (which is already a symbol node).
func TestChunkFile_EmitsChunksForUncoveredRanges(t *testing.T) {
	src := []byte(strings.Repeat("line\n", 200))
	symbols := []*domain.Node{
		mustNode(t, "sym1", "f.go", "Foo", domain.KindFunction, domain.LineRange{Start: 10, End: 15}),
	}
	chunks := chunkFile("repo", "f.go", src, symbols)
	if len(chunks) == 0 {
		t.Fatal("expected chunk nodes for uncovered regions, got none")
	}
	for _, c := range chunks {
		if c.Kind != domain.KindChunk {
			t.Errorf("chunk %q has kind %q, want %q", c.ID, c.Kind, domain.KindChunk)
		}
		// No chunk should overlap the symbol's [10,15] range — covered
		// regions are skipped.
		if c.Lines.Start <= 15 && c.Lines.End >= 10 {
			t.Errorf("chunk %s [%d,%d] overlaps symbol [10,15]",
				c.ID, c.Lines.Start, c.Lines.End)
		}
	}
}

// TestChunkFile_SkipsWhitespaceOnlyGaps guards solov2-wh7u: a blank-line gap
// between two symbols must not become a chunk node. Whitespace-only chunks
// embed to near-anything and outrank real code in search results.
func TestChunkFile_SkipsWhitespaceOnlyGaps(t *testing.T) {
	src := []byte("type A struct{}\n\n\ntype B struct{}\n")
	symbols := []*domain.Node{
		mustNode(t, "symA", "f.go", "A", domain.KindStruct, domain.LineRange{Start: 1, End: 1}),
		mustNode(t, "symB", "f.go", "B", domain.KindStruct, domain.LineRange{Start: 4, End: 4}),
	}
	chunks := chunkFile("repo", "f.go", src, symbols)
	for _, c := range chunks {
		if c.RawContent == nil || strings.TrimSpace(*c.RawContent) == "" {
			t.Errorf("emitted whitespace-only chunk %s [%d,%d]", c.ID, c.Lines.Start, c.Lines.End)
		}
	}
}

// TestChunkFile_NoSymbolsChunksWholeFile: a documentation-only file
// (no symbols) should be entirely covered by chunks. Without this,
// READMEs, top-of-file commentary, and non-declarative TS modules
// would be invisible to semantic search.
func TestChunkFile_NoSymbolsChunksWholeFile(t *testing.T) {
	src := []byte(strings.Repeat("line\n", 100))
	chunks := chunkFile("repo", "f.go", src, nil)
	if len(chunks) == 0 {
		t.Fatal("expected chunks for a no-symbol file")
	}
	// At chunkLineWindow=80, 100 lines yields 2 chunks: [1,80] and [81,100].
	if len(chunks) != 2 {
		t.Errorf("100-line file at 80-line windows: got %d chunks, want 2", len(chunks))
	}
	if chunks[0].Lines.Start != 1 || chunks[0].Lines.End != 80 {
		t.Errorf("first chunk: got [%d,%d], want [1,80]",
			chunks[0].Lines.Start, chunks[0].Lines.End)
	}
}

// TestChunkFile_SnippetPopulated: each chunk must carry its source
// bytes via raw_content so the embedder and FTS pipeline can index
// it. Without this, chunks would be empty rows.
func TestChunkFile_SnippetPopulated(t *testing.T) {
	src := []byte("a\nb\nc\nd\ne\n")
	chunks := chunkFile("repo", "f.go", src, nil)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for a 5-line file, got %d", len(chunks))
	}
	if chunks[0].RawContent == nil || *chunks[0].RawContent == "" {
		t.Errorf("chunk raw_content is empty")
	}
	if !strings.Contains(*chunks[0].RawContent, "a\n") {
		t.Errorf("chunk raw_content missing source bytes: %q", *chunks[0].RawContent)
	}
}

// TestChunkFile_DeterministicIDs: two passes over the same input
// must produce byte-identical chunk IDs so promotion is idempotent.
func TestChunkFile_DeterministicIDs(t *testing.T) {
	src := []byte(strings.Repeat("x\n", 250))
	a := chunkFile("repo", "f.go", src, nil)
	b := chunkFile("repo", "f.go", src, nil)
	if len(a) != len(b) {
		t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("chunk %d id differs: %q vs %q", i, a[i].ID, b[i].ID)
		}
	}
}

// TestChunkFile_FullyCoveredFileEmitsNoChunks: when symbols already
// cover the entire file (line-by-line), no chunks should be emitted
// — the chunk index is for non-declaration code only.
func TestChunkFile_FullyCoveredFileEmitsNoChunks(t *testing.T) {
	src := []byte(strings.Repeat("x\n", 50))
	symbols := []*domain.Node{
		mustNode(t, "s", "f.go", "S", domain.KindFunction, domain.LineRange{Start: 1, End: 50}),
	}
	chunks := chunkFile("repo", "f.go", src, symbols)
	if len(chunks) != 0 {
		t.Errorf("fully-covered file should emit no chunks, got %d", len(chunks))
	}
}

func mustNode(t *testing.T, id, path, name string, kind domain.NodeKind, lr domain.LineRange) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind}, domain.WithLines(lr))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return n
}
