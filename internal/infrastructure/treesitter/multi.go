// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// extParser defines a CodeParser that can enumerate the file extensions it parses.
type extParser interface {
	ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error)
	SupportedExtensions() []string
}

// MultiParser routes ParseFile requests to the sub-parser registered for a file's
// extension. It is safe for concurrent use.
type MultiParser struct {
	byExt map[string]extParser
}

// NewMultiParser builds a MultiParser instance. If multiple parsers claim the same
// file extension, the last one registered takes precedence.
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
