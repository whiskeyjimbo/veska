package treesitter

import (
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Parser pools amortize the CGO allocation costs of sitter.NewParser and SetLanguage
// across multiple files. Since tree-sitter parsers are bound to a single language after
// initialization, we maintain one sync.Pool per language. Parser instances are safe to
// reuse without reset because each call uses a nil old tree.
var (
	goParserPool  = newParserPool(golang.GetLanguage())
	tsParserPool  = newParserPool(typescript.GetLanguage())
	tsxParserPool = newParserPool(tsx.GetLanguage())
)

func newParserPool(lang *sitter.Language) *sync.Pool {
	return &sync.Pool{
		New: func() any {
			p := sitter.NewParser()
			p.SetLanguage(lang)
			return p
		},
	}
}
