package treesitter

import (
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Parser pools amortize the per-language CGO setup (sitter.NewParser +
// SetLanguage) across files. ParseFile is hot under bulk re-index and the
// fsnotify watcher; allocating a fresh parser per call was wasteful
// One sync.Pool per language because tree-sitter parsers are
// bound to a single language after SetLanguage.
// Safety: Parser.ParseCtx is called with a nil "old tree" each time, so a
// parse never depends on the parser's previous state, and parsers returned to
// the pool can be reused without an explicit reset. Each call holds a parser
// exclusively for the duration of ParseFile (Get/defer Put), so concurrent
// ParseFile calls each get their own parser from the pool.
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
