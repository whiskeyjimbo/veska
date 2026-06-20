// SPDX-License-Identifier: AGPL-3.0-only

// realcorpus.go builds a recall-projection corpus from a real Go module
// instead of the synthetic corpus.
// Background: the synthetic sweep's +snippet
// input is synthSnippet - a restatement of the cluster query text - so
// its recall figure is circular. A faithful measurement needs real
// source bodies and queries that are written independently of them.
// BuildRealCorpus walks a Go module with go/parser + go/doc and emits a
// ProjectionCorpus where, for each exported symbol:
//   Snippet = the symbol's real source body (no doc comment).
//   Signature = the symbol's real declaration line.
//   Kind/SymbolPath/FilePath = real structural identifiers.
//   the center query (documented symbols only) = the symbol's doc
//     comment - natural-language prose written separately from the body.
// Each documented symbol is its own cluster with single-node ground
// truth: recall@10 asks whether a symbol's doc-comment query retrieves
// that symbol out of the whole corpus. Undocumented exported symbols are
// carried as non-queried distractors so they still compete in search.

package recallprojection

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// maxSnippetBytes caps a node's source snippet. nomic-embed-text has a
// bounded context; a snippet - not a whole 500-line function - is what a
// realistic projection would carry, and the cap keeps embed cost uniform.
const maxSnippetBytes = 2000

// realSymbol is one exported symbol harvested from the module before it
// is assigned a cluster.
type realSymbol struct {
	input domain.EmbedTextInput
	doc   string // doc comment; "" for an undocumented distractor
}

// BuildRealCorpus parses every non-test Go package under root and returns
// a ProjectionCorpus. Documented exported symbols become queried clusters;
// undocumented exported symbols become distractors. It returns an error
// only for an unreadable root; unparseable individual directories are
// skipped so one bad file does not abort the corpus.
func BuildRealCorpus(root string) (ProjectionCorpus, error) {
	dirs, err := goPackageDirs(root)
	if err != nil {
		return ProjectionCorpus{}, err
	}
	var syms []realSymbol
	for _, dir := range dirs {
		syms = append(syms, symbolsInDir(root, dir)...)
	}
	return assembleCorpus(syms), nil
}

// goPackageDirs returns every directory under root that holds at least one
// non-test.go file, skipping vendor, testdata, and dot-directories.
func goPackageDirs(root string) ([]string, error) {
	seen := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "testdata" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			seen[filepath.Dir(path)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("realcorpus: walk %s: %w", root, err)
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // deterministic corpus order
	return dirs, nil
}

// symbolsInDir parses one directory and harvests its exported funcs,
// methods, and types. A directory that fails to parse yields no symbols.
func symbolsInDir(root, dir string) []realSymbol {
	entries, err := os.ReadDir(dir) // os.ReadDir returns entries sorted by name
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	byPkg := make(map[string][]*ast.File)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if perr != nil || f.Name == nil {
			continue
		}
		byPkg[f.Name.Name] = append(byPkg[f.Name.Name], f)
	}
	pkgNames := make([]string, 0, len(byPkg))
	for name := range byPkg {
		pkgNames = append(pkgNames, name)
	}
	sort.Strings(pkgNames)

	var out []realSymbol
	for _, name := range pkgNames {
		// PreserveAST: without it doc.NewFromFiles clears function
		// bodies, leaving nothing for the real-source snippet.
		dpkg, derr := doc.NewFromFiles(fset, byPkg[name], "./"+name, doc.PreserveAST)
		if derr != nil {
			continue
		}
		for _, fn := range dpkg.Funcs {
			if s, ok := funcSymbol(root, fset, name, "", fn); ok {
				out = append(out, s)
			}
		}
		for _, typ := range dpkg.Types {
			if s, ok := typeSymbol(root, fset, name, typ); ok {
				out = append(out, s)
			}
			for _, m := range typ.Methods {
				if s, ok := funcSymbol(root, fset, name, typ.Name, m); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// funcSymbol turns a documented-or-not func/method into a realSymbol.
func funcSymbol(root string, fset *token.FileSet, pkg, recv string, fn *doc.Func) (realSymbol, bool) {
	decl := fn.Decl
	if decl == nil {
		return realSymbol{}, false
	}
	src, file := nodeSource(root, fset, decl.Pos(), decl.End())
	if src == "" {
		return realSymbol{}, false
	}
	sigEnd := decl.End()
	if decl.Body != nil {
		sigEnd = decl.Body.Pos()
	}
	sig, _ := nodeSource(root, fset, decl.Pos(), sigEnd)
	kind, symbol := "function", pkg+"."+fn.Name
	if recv != "" {
		kind, symbol = "method", pkg+"."+recv+"."+fn.Name
	}
	return realSymbol{
		input: domain.EmbedTextInput{
			Kind:       kind,
			SymbolPath: symbol,
			FilePath:   file,
			Language:   "go",
			Signature:  strings.TrimSpace(sig),
			Snippet:    capSnippet(src),
		},
		doc: strings.TrimSpace(fn.Doc),
	}, true
}

// typeSymbol turns a type declaration into a realSymbol.
func typeSymbol(root string, fset *token.FileSet, pkg string, typ *doc.Type) (realSymbol, bool) {
	if typ.Decl == nil {
		return realSymbol{}, false
	}
	src, file := nodeSource(root, fset, typ.Decl.Pos(), typ.Decl.End())
	if src == "" {
		return realSymbol{}, false
	}
	return realSymbol{
		input: domain.EmbedTextInput{
			Kind:       "type",
			SymbolPath: pkg + "." + typ.Name,
			FilePath:   file,
			Language:   "go",
			Signature:  strings.TrimSpace(firstLine(src)),
			Snippet:    capSnippet(src),
		},
		doc: strings.TrimSpace(typ.Doc),
	}, true
}

// nodeSource reads the source text spanning [pos, end) and returns it with
// the root-relative file path. An unreadable file yields ("", "").
func nodeSource(root string, fset *token.FileSet, pos, end token.Pos) (string, string) {
	p, e := fset.Position(pos), fset.Position(end)
	if p.Filename == "" || p.Offset < 0 || e.Offset < p.Offset {
		return "", ""
	}
	b, err := os.ReadFile(p.Filename)
	if err != nil || e.Offset > len(b) {
		return "", ""
	}
	rel, relErr := filepath.Rel(root, p.Filename)
	if relErr != nil {
		rel = p.Filename
	}
	return string(b[p.Offset:e.Offset]), rel
}

// assembleCorpus assigns clusters: each documented symbol is its own
// queried cluster with single-node truth; undocumented symbols share one
// trailing distractor cluster that is never queried.
func assembleCorpus(syms []realSymbol) ProjectionCorpus {
	var documented, distractors []realSymbol
	for _, s := range syms {
		if s.doc != "" {
			documented = append(documented, s)
		} else {
			distractors = append(distractors, s)
		}
	}
	corpus := ProjectionCorpus{
		Clusters:      len(documented),
		Nodes:         make([]ProjectionNode, 0, len(syms)),
		CenterQueries: make([]string, 0, len(documented)),
	}
	for i, s := range documented {
		corpus.Nodes = append(corpus.Nodes, ProjectionNode{
			NodeID:  fmt.Sprintf("rc-%05d", len(corpus.Nodes)),
			Cluster: i,
			Input:   s.input,
		})
		corpus.CenterQueries = append(corpus.CenterQueries, s.doc)
	}
	if len(distractors) > 0 {
		distractorCluster := corpus.Clusters // index past every queried cluster
		corpus.Clusters++
		for _, s := range distractors {
			corpus.Nodes = append(corpus.Nodes, ProjectionNode{
				NodeID:  fmt.Sprintf("rc-%05d", len(corpus.Nodes)),
				Cluster: distractorCluster,
				Input:   s.input,
			})
		}
	}
	return corpus
}

// capSnippet trims a snippet to maxSnippetBytes on a rune boundary so the
// embed input stays bounded and uniform across nodes.
func capSnippet(s string) string {
	if len(s) <= maxSnippetBytes {
		return s
	}
	cut := maxSnippetBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8RuneStart reports whether b is a UTF-8 leading byte.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// firstLine returns s up to the first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
