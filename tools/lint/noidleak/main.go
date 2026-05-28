// Command noidleak fails the build when internal bd issue IDs (solov2-xxxx)
// leak into user-visible Go string literals — cobra flag descriptions,
// fmt.Printf format strings, MCP tool descriptions, generated docs.
//
// Background: bd issue IDs are useful breadcrumbs in code comments (where
// they tag the commit history) but read as cryptic noise when they reach
// --help text or wiki pages. The lint exists because a junior-eng journey
// (solov2-a0hw) kept tripping over leaked IDs in init output, flag help,
// and entry_points.md.
//
// Walks every .go file under cmd/ and internal/ (skipping _test.go), parses
// each one, visits every basic-string literal, and flags any match of
// /solov2-[a-z0-9.]+/. Comments are not visited — they live outside the
// AST's value-expression walk by design.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var idRe = regexp.MustCompile(`solov2-[a-z0-9.]+`)

func main() {
	roots := []string{"cmd", "internal"}
	var violations []string
	for _, root := range roots {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			v, err := scanFile(path)
			if err != nil {
				return err
			}
			violations = append(violations, v...)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "walk %s: %v\n", root, err)
			os.Exit(2)
		}
	}
	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "noidleak: internal bd issue IDs found in user-visible string literals:")
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "These reach --help text, generated docs, and tool descriptions. Move the")
		fmt.Fprintln(os.Stderr, "id into a code comment (// solov2-xxxx) instead.")
		os.Exit(1)
	}
}

func scanFile(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if idRe.MatchString(val) {
			pos := fset.Position(lit.Pos())
			out = append(out, fmt.Sprintf("%s:%d: string literal contains %q", pos.Filename, pos.Line, idRe.FindString(val)))
		}
		return true
	})
	return out, nil
}
