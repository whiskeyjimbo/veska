package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// extParser is a CodeParser that can also enumerate the extensions it parses.
// Both GoParser and TSParser satisfy it; MultiParser routes by extension and
// exposes the union of its members' SupportedExtensions.
type extParser interface {
	ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error)
	SupportedExtensions() []string
}

// MultiParser routes ParseFile to the sub-parser that claims a file's
// extension and reports the union of its sub-parsers' supported extensions.
// It lets the cold scan parse several languages through one ports.CodeParser
// while sourcing its walk filter from SupportedExtensions instead of a
// hand-synced list (solov2-xde2.7). A file whose extension no sub-parser
// claims yields an empty ParseResult — the same contract each sub-parser
// already honours for unrecognised extensions. Safe for concurrent use:
// byExt is read-only after construction.
type MultiParser struct {
	byExt map[string]extParser
}

// NewMultiParser builds a MultiParser routing each parser's
// SupportedExtensions to it. When two parsers claim the same extension the
// last one wins; production wires disjoint sets (Go vs TS/TSX).
func NewMultiParser(parsers ...extParser) *MultiParser {
	byExt := make(map[string]extParser)
	for _, p := range parsers {
		for _, ext := range p.SupportedExtensions() {
			byExt[strings.ToLower(ext)] = p
		}
	}
	return &MultiParser{byExt: byExt}
}

// ParseFile dispatches to the sub-parser registered for the file's extension,
// returning an empty ParseResult when none matches.
func (m *MultiParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if p, ok := m.byExt[ext]; ok {
		return p.ParseFile(ctx, repoID, path, src)
	}
	return &domain.ParseResult{}, nil
}

// SupportedExtensions returns the sorted union of every sub-parser's
// extensions.
func (m *MultiParser) SupportedExtensions() []string {
	exts := make([]string, 0, len(m.byExt))
	for ext := range m.byExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}
