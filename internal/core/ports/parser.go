package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// CodeParser is the port for parsing source files into domain graph elements.
// Implementations are provided by infrastructure adapters (e.g. tree-sitter).
type CodeParser interface {
	// ParseFile parses the source bytes of the file at path and returns the
	// Nodes and Edges extracted from it. repoID is used to scope any IDs that
	// embed a repository prefix. src must be valid UTF-8.
	ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error)
}
