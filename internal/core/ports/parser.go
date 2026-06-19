// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// CodeParser parses source files into domain graph elements.
type CodeParser interface {
	// ParseFile parses the source bytes of a file. The src slice must be valid UTF-8.
	ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error)
}
