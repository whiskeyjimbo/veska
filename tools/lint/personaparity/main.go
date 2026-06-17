// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Command personaparity enforces the persona-suite coverage contract
// every eng_* MCP tool registered in
// internal/infrastructure/mcp/ must be EXERCISED by some test under
// tests/mcp/ - either a persona workflow (tests/mcp/test_persona_*.py,
// tests/mcp/persona_harness.py) or the per-tool suite (any other
// tests/mcp/*.py) - OR be listed in tools/lint/personaparity/parked.txt
// with a reason.
// Unlike a hand-maintained allow-list, the gate VERIFIES references: it
// greps the test corpus for each tool name, so a tool that no test names
// fails the gate. A new tool added to the registry without a covering test
// turns this red; deleting a tool's last test reference does too. This is
// the "test ALL functionality" guarantee for the eng_* surface.
// CLI/MCP parity (every tool also reachable via a `veska` subcommand) is a
// separate concern enforced by the cliparity lint; the CLI wraps these same
// tools, so personaparity scopes itself to the MCP surface.
// The MCP scan walks *.go via go/ast (no daemon spin-up), so this runs in
// the same pre-merge gate as gofmt and `go vet`.
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
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	mcpDir     = "internal/infrastructure/mcp"
	testDir    = "tests/mcp"
	parkedFile = "tools/lint/personaparity/parked.txt"
)

// personaFiles are the test files that count a tool as persona-covered (as
// opposed to delegated to the per-tool suite). Reporting only - both kinds
// satisfy the gate.
var personaFiles = map[string]struct{}{
	"persona_harness.py":      {},
	"test_persona_harness.py": {},
	"test_persona_junior.py":  {},
	"test_persona_senior.py":  {},
	"test_persona_agent.py":   {},
}

// engToolRe matches a tool name only as a quoted string literal - the way a
// tool is actually invoked over MCP (mcp.call("eng_x",.)). Bare mentions in
// comments/docstrings (e.g. a PARKED note naming a tool) are deliberately NOT
// counted as coverage.
var engToolRe = regexp.MustCompile(`"(eng_[a-z_]+)"`)

func main() {
	tools, err := scanRegisteredTools()
	if err != nil {
		fail("scan registrations: %v", err)
	}
	refs, err := scanTestRefs()
	if err != nil {
		fail("scan test refs: %v", err)
	}
	parked, err := readParked(parkedFile)
	if err != nil {
		fail("read parked manifest: %v", err)
	}

	registered := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		registered[t] = struct{}{}
	}

	var uncovered, staleParked []string
	persona, delegated := 0, 0
	for _, t := range tools {
		if _, isParked := parked[t]; isParked {
			continue
		}
		files := refs[t]
		if len(files) == 0 {
			uncovered = append(uncovered, t)
			continue
		}
		if referencedByPersona(files) {
			persona++
		} else {
			delegated++
		}
	}
	// A parked tool that is actually exercised (or no longer registered) means
	// the manifest has rotted - surface it so parked.txt stays honest.
	for name := range parked {
		if _, ok := registered[name]; !ok {
			staleParked = append(staleParked, name+" (no longer registered)")
			continue
		}
		if files := refs[name]; referencedByPersona(files) {
			staleParked = append(staleParked, name+" (now persona-covered - unpark it)")
		}
	}

	if len(uncovered) == 0 && len(staleParked) == 0 {
		fmt.Printf("personaparity: OK - %d tools (%d persona-covered, %d delegated, %d parked)\n",
			len(tools), persona, delegated, len(parked))
		return
	}

	fmt.Fprintln(os.Stderr, "personaparity: persona-suite coverage drift detected")
	if len(uncovered) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "MCP tools exercised by NO test under tests/mcp/ (add a persona")
		fmt.Fprintln(os.Stderr, "or per-tool test, or list in tools/lint/personaparity/parked.txt):")
		sort.Strings(uncovered)
		for _, t := range uncovered {
			fmt.Fprintf(os.Stderr, "  %s\n", t)
		}
	}
	if len(staleParked) > 0 {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Stale parked.txt entries:")
		sort.Strings(staleParked)
		for _, s := range staleParked {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}
	os.Exit(1)
}

func referencedByPersona(files map[string]struct{}) bool {
	for f := range files {
		if _, ok := personaFiles[f]; ok {
			return true
		}
	}
	return false
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "personaparity: "+format+"\n", args...)
	os.Exit(2)
}

// scanRegisteredTools walks internal/infrastructure/mcp/*.go and extracts the
// Name of every ToolSpec composite literal passed to r.MustRegister or
// r.Register. Test files are excluded - test-only registrations aren't part of
// the shipped surface. Parked-but-compiled tools (RegisterTaskTools) ARE
// returned: they live in a non-test file, so parked.txt must account for them.
func scanRegisteredTools() ([]string, error) {
	var out []string
	err := filepath.WalkDir(mcpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
			if !ok || (sel.Sel.Name != "MustRegister" && sel.Sel.Name != "Register") {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			if name := toolSpecName(lit); name != "" {
				out = append(out, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// toolSpecName returns the eng_* string assigned to the Name field of a
// ToolSpec composite literal, or "" if the literal has no string Name.
func toolSpecName(lit *ast.CompositeLit) string {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Name" {
			continue
		}
		val, ok := kv.Value.(*ast.BasicLit)
		if !ok || val.Kind != token.STRING {
			continue
		}
		s, err := strconv.Unquote(val.Value)
		if err == nil && strings.HasPrefix(s, "eng_") {
			return s
		}
	}
	return ""
}

// scanTestRefs maps each eng_* tool name to the set of tests/mcp basenames
// that reference it (string-literal grep over *.py). A reference is any
// occurrence of the tool name - call, comment, or fixture - which is the
// honest signal that a test names the tool.
func scanTestRefs() (map[string]map[string]struct{}, error) {
	out := make(map[string]map[string]struct{})
	entries, err := os.ReadDir(testDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(testDir, e.Name()))
		if rerr != nil {
			return nil, rerr
		}
		for _, m := range engToolRe.FindAllStringSubmatch(string(data), -1) {
			name := m[1]
			if out[name] == nil {
				out[name] = make(map[string]struct{})
			}
			out[name][e.Name()] = struct{}{}
		}
	}
	return out, nil
}

// readParked reads the parked manifest: one `tool_name reason` per line,
// blank lines and #-comments ignored. The reason is required (a parked tool
// must justify why it is unexercised).
func readParked(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, ok := strings.Cut(line, " ")
		if !ok || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: expected `tool_name reason`, got %q", path, ln, line)
		}
		out[name] = strings.TrimSpace(reason)
	}
	return out, sc.Err()
}
