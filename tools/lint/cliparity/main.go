// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Command cliparity enforces the CLI/MCP surface-parity contract
// Every MCP tool registered in internal/infrastructure/mcp/
// must EITHER:
//
//	Appear in tools/lint/cliparity/wrapped.txt (the manifest of tools
//	  that have a `veska <subcommand>` wrapper), OR
//	Carry a non-zero CLIExempt value on its ToolSpec literal
//
// Stale manifest entries (names with no matching MCP registration) are
// also flagged so the manifest stays a live document rather than a
// graveyard. The lint walks internal/infrastructure/mcp/*.go via the
// go/ast package - no daemon spin-up required - so it runs in the same
// pre-merge gate as gofmt and `go vet`.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	mcpDir       = "internal/infrastructure/mcp"
	manifestPath = "tools/lint/cliparity/wrapped.txt"
)

type toolFact struct {
	name   string
	exempt string // CLIExempt enum identifier (empty = none)
	reason string // ExemptReason literal (empty = none)
	file   string
	line   int
}

func main() {
	tools, err := scanMCPRegistrations()
	if err != nil {
		fail("scan registrations: %v", err)
	}
	wrapped, err := readManifest(manifestPath)
	if err != nil {
		fail("read manifest: %v", err)
	}

	wrappedSet := make(map[string]struct{}, len(wrapped))
	for _, w := range wrapped {
		wrappedSet[w] = struct{}{}
	}
	toolByName := make(map[string]toolFact, len(tools))
	for _, t := range tools {
		toolByName[t.name] = t
	}

	var uncovered []toolFact
	for _, t := range tools {
		if _, w := wrappedSet[t.name]; w {
			continue
		}
		if t.exempt != "" && t.exempt != "CLIExemptNone" {
			continue
		}
		uncovered = append(uncovered, t)
	}

	var stale []string
	for _, w := range wrapped {
		if _, ok := toolByName[w]; !ok {
			stale = append(stale, w)
		}
	}

	if len(uncovered) == 0 && len(stale) == 0 {
		return
	}

	fmt.Fprintln(os.Stderr, "cliparity: CLI/MCP surface drift detected")
	if len(uncovered) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "MCP tools missing a CLI wrapper (add to tools/lint/cliparity/wrapped.txt")
		fmt.Fprintln(os.Stderr, "or mark CLIExempt on the ToolSpec):")
		sort.Slice(uncovered, func(i, j int) bool { return uncovered[i].name < uncovered[j].name })
		for _, t := range uncovered {
			fmt.Fprintf(os.Stderr, "  %s  (%s:%d)\n", t.name, t.file, t.line)
		}
	}
	if len(stale) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Stale manifest entries (tool no longer registered - remove from wrapped.txt):")
		sort.Strings(stale)
		for _, s := range stale {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}
	os.Exit(1)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cliparity: "+format+"\n", args...)
	os.Exit(2)
}

// scanMCPRegistrations walks internal/infrastructure/mcp/*.go and
// extracts every ToolSpec composite literal passed to r.MustRegister
// (or r.Register). Returns the (Name, CLIExempt, ExemptReason) triple
// for each. Tests are excluded - test-only registrations aren't part
// of the shipped surface.
func scanMCPRegistrations() ([]toolFact, error) {
	var out []toolFact
	err := filepath.WalkDir(mcpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "MustRegister" && sel.Sel.Name != "Register" {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			// Best-effort: only inspect ToolSpec{.} literals.
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "ToolSpec" {
				return true
			}
			t := toolFact{file: path, line: fset.Position(lit.Pos()).Line}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Name":
					if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if v, err := strconv.Unquote(bl.Value); err == nil {
							t.name = v
						}
					}
				case "CLIExempt":
					if id, ok := kv.Value.(*ast.Ident); ok {
						t.exempt = id.Name
					}
				case "ExemptReason":
					if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if v, err := strconv.Unquote(bl.Value); err == nil {
							t.reason = v
						}
					}
				}
			}
			if t.name != "" {
				out = append(out, t)
			}
			return true
		})
		return nil
	})
	return out, err
}

// readManifest loads wrapped.txt, stripping comments and blank lines.
func readManifest(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
