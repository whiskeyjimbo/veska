package sqlite

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// maxSnippetBytes bounds the node body persisted into nodes.snippet. The body
// feeds embed-text projection, so it is capped to keep embed cost and snippet
// storage bounded and uniform — matching the recallprojection harness cap.
const maxSnippetBytes = 2000

// capSnippet trims s to at most maxSnippetBytes on a UTF-8 rune boundary so the
// stored snippet never contains a broken rune.
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

// nodeSnippet returns the SQL bind value for the nodes.snippet column —
// capped RawContent when the parser populated it, otherwise nil (NULL in
// SQLite). Shared between GraphRepo.SaveNode and PromotionStore.Promote so
// the embed-text projection has the same body in both write paths
// .
func nodeSnippet(n *domain.Node) any {
	if n == nil || n.RawContent == nil {
		return nil
	}
	return capSnippet(*n.RawContent)
}
