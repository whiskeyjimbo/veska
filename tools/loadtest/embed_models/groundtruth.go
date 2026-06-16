//go:build eval

// Ground-truth extractors for the embed-models bench. Three sources
//   Doc-derived (cheap, bulk): for each exported declaration with a
//     non-empty doc comment, emit (paraphrase-of-comment, symbol-name).
//   Hand-curated headline.jsonl: ~20 natural-language queries per
//     corpus, the PUBLISHED headline metric.
//   Test-name-derived (auxiliary): TestXxx_Yyy → (humanized "Yyy",
//     tested-symbol "Xxx").
// All sources produce Pair so the recall metric in recall.go runs
// each uniformly. Symbol names follow the treesitter parser's
// convention so they line up with the embedded doc names: methods are
// "Receiver.Method"; top-level decls are bare.

package embed_models

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Pair is one (query, expected-symbol) ground-truth entry.
type Pair struct {
	Query    string `json:"query"`
	Expected string `json:"expected"`
}

// GTSource is a named bundle of pairs scoped to one corpus.
type GTSource struct {
	Name  string // "doc", "headline", "test-name"
	Pairs []Pair
}

// CollectGroundTruth returns every available source for the corpus, in
// order: headline (loaded from every *.jsonl file in fixturesDir), doc
// (auto-extracted from Go source — code corpora only), test-name
// (auto-extracted from _test.go — code corpora only). Empty sources
// are returned with an empty Pairs slice so per-source recall rows are
// still emitted. kind toggles the doc/test-name sources on/off — they
// only make sense for code corpora.
func CollectGroundTruth(corpusName, corpusRoot, fixturesDir, kind string) []GTSource {
	headline, _ := loadHeadlineDir(fixturesDir, corpusName)
	sources := []GTSource{{Name: "headline", Pairs: headline}}
	if kind == "code" {
		sources = append(sources,
			GTSource{Name: "doc", Pairs: docDerived(corpusRoot)},
			GTSource{Name: "test-name", Pairs: testNameDerived(corpusRoot)},
		)
	}
	return sources
}

// ──────────────────────────────────────────────────────────────────────
// Doc-derived
// ──────────────────────────────────────────────────────────────────────

// docDerived parses every.go file under root with go/parser
// (ParseComments) and emits one Pair per exported function/method/type
// with a non-empty doc comment. The query is the first sentence of the
// doc with the symbol's name stripped from the front (Go's convention
// is "Foo does X" — keeping "Foo" leaks the answer into the query).
// Symbol naming matches treesitter's convention so recall can compare
// directly against the embedded docs' name field:
//
//	method on receiver T → "T.Method"
//	everything else → bare name (function/type/struct/etc.)
func docDerived(root string) []Pair {
	var out []Pair
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || name == "out" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil // tolerate parse errors per-file
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Doc == nil || d.Name == nil {
					continue
				}
				if !d.Name.IsExported() {
					continue
				}
				sym := funcSymbolName(d)
				q := firstSentenceMinusPrefix(d.Doc.Text(), d.Name.Name)
				if q == "" {
					continue
				}
				out = append(out, Pair{Query: q, Expected: sym})
			case *ast.GenDecl:
				if d.Doc == nil {
					continue
				}
				// type/var/const declarations — emit one pair per
				// exported type spec only (vars/consts rarely have
				// search-worthy individual docs).
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name == nil || !ts.Name.IsExported() {
						continue
					}
					q := firstSentenceMinusPrefix(d.Doc.Text(), ts.Name.Name)
					if q == "" {
						continue
					}
					out = append(out, Pair{Query: q, Expected: ts.Name.Name})
				}
			}
		}
		return nil
	})
	if err != nil {
		return out // walk errors are non-fatal for the bench
	}
	return out
}

// funcSymbolName matches the treesitter parser's node-naming
// convention: methods become "Receiver.Method", functions stay bare.
func funcSymbolName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	// Extract receiver type — strip the pointer if present.
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return d.Name.Name
	}
	return id.Name + "." + d.Name.Name
}

// firstSentenceMinusPrefix returns the first sentence of comment with
// any leading occurrence of symbolName stripped — Go's "Foo does X"
// convention would otherwise leak the answer into the query.
func firstSentenceMinusPrefix(comment, symbolName string) string {
	text := strings.TrimSpace(comment)
	if text == "" {
		return ""
	}
	// First sentence: up to the first '.' followed by whitespace or end.
	end := len(text)
	for i := 0; i < len(text); i++ {
		if text[i] != '.' {
			continue
		}
		if i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\n' || text[i+1] == '\t' {
			end = i
			break
		}
	}
	sentence := strings.TrimSpace(text[:end])

	// Strip "<symbolName> " prefix if present (case-sensitive — Go
	// convention starts the doc with the exact name).
	prefix := symbolName + " "
	if strings.HasPrefix(sentence, prefix) {
		sentence = strings.TrimSpace(sentence[len(prefix):])
	}
	// Collapse internal whitespace so multi-line first sentences
	// become single-line queries.
	sentence = strings.Join(strings.Fields(sentence), " ")
	return sentence
}

// ──────────────────────────────────────────────────────────────────────
// Headline (hand-curated)
// ──────────────────────────────────────────────────────────────────────

// headlineEntry is one row in fixtures/headline.jsonl.
type headlineEntry struct {
	Corpus   string `json:"corpus"`
	Query    string `json:"query"`
	Expected string `json:"expected"`
}

// loadHeadlineDir reads every *.jsonl file in fixturesDir and returns
// the union of pairs scoped to corpusName. Missing files / dirs are
// non-errors (returns empty slice) so the bench is runnable before
// hand-curated fixtures exist for a corpus.
func loadHeadlineDir(fixturesDir, corpusName string) ([]Pair, error) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Pair
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		pairs, err := loadHeadline(filepath.Join(fixturesDir, e.Name()), corpusName)
		if err != nil {
			continue // tolerate per-file failures
		}
		out = append(out, pairs...)
	}
	return out, nil
}

// loadHeadline reads a single.jsonl file and returns the pairs scoped
// to corpusName. Missing file is a non-error.
func loadHeadline(path, corpusName string) ([]Pair, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Pair
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var e headlineEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate malformed lines
		}
		if e.Corpus != corpusName {
			continue
		}
		out = append(out, Pair{Query: e.Query, Expected: e.Expected})
	}
	return out, scanner.Err()
}

// ──────────────────────────────────────────────────────────────────────
// Test-name-derived
// ──────────────────────────────────────────────────────────────────────

// testNameDerived parses _test.go files; for each Test<Symbol>_<Suffix>
// function, emits (humanized "<Suffix>", "<Symbol>"). Lower-quality
// signal than the other two sources but free and gives a sanity check
// against the production code's own self-description.
func testNameDerived(root string) []Pair {
	var out []Pair
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "out" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil || fd.Recv != nil {
				continue
			}
			name := fd.Name.Name
			if !strings.HasPrefix(name, "Test") {
				continue
			}
			rest := strings.TrimPrefix(name, "Test")
			parts := strings.SplitN(rest, "_", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				continue
			}
			expected := parts[0]
			query := humanize(parts[1])
			if query == "" {
				continue
			}
			out = append(out, Pair{Query: query, Expected: expected})
		}
		return nil
	})
	if err != nil {
		return out
	}
	return out
}

// humanize converts a CamelCase test-name suffix to a lowercase
// space-separated phrase: "ReturnsThreeBuckets" → "returns three buckets".
func humanize(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
