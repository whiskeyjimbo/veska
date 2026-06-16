package sqlite

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// maxSnippetBytes limits the node body size persisted in nodes.snippet. The snippet
// is capped to keep embedding costs and snippet storage bounded, matching the
// recallprojection harness limit.
const maxSnippetBytes = 2000

// capSnippet trims s to at most maxSnippetBytes on a UTF-8 rune boundary to
// avoid storing a broken rune.
func capSnippet(s string) string {
	if len(s) <= maxSnippetBytes {
		return s
	}
	cut := maxSnippetBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// nodeSnippet returns the SQL bind value for the nodes.snippet column, capping
// the raw content at maxSnippetBytes. This ensures both GraphRepo and PromotionStore
// write the same snippet format for text embedding.
func nodeSnippet(n *domain.Node) any {
	if n == nil || n.RawContent == nil {
		return nil
	}
	return capSnippet(*n.RawContent)
}
