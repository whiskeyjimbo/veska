// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// These helpers map a domain.Node's optional fields to the nullable SQL column
// values the promotion INSERT binds. Pointer-nil becomes SQL NULL (or the empty
// string for the non-null text columns); bool becomes 0/1.

func nodeLanguage(n *domain.Node) string {
	if n.Language == nil {
		return ""
	}
	return *n.Language
}

func nodeLines(n *domain.Node) (lineStart, lineEnd any) {
	if n.Lines == nil {
		return nil, nil
	}
	return n.Lines.Start, n.Lines.End
}

func nodeStructuralHash(n *domain.Node) any {
	if n.StructuralHash == nil {
		return nil
	}
	return string(*n.StructuralHash)
}

func nodeContentHash(n *domain.Node) string {
	if n.ContentHash == nil {
		return ""
	}
	return string(*n.ContentHash)
}

func nodeSignature(n *domain.Node) any {
	if n.Signature == nil {
		return nil
	}
	return *n.Signature
}

func nodeExported(n *domain.Node) any {
	if n.Exported == nil {
		return nil
	}
	if *n.Exported {
		return 1
	}
	return 0
}
