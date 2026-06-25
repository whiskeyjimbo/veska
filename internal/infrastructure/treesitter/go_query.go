// SPDX-License-Identifier: AGPL-3.0-only

// Package treesitter implements a query-driven Go parser utilizing tree-sitter
// queries to extract declarations and call relationships.

package treesitter

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GoParser is the query-driven implementation of CodeParser for Go. It is safe for
// concurrent use.
type GoParser struct{}

// NewGoParser constructs a new GoParser instance.
func NewGoParser() *GoParser {
	return &GoParser{}
}

// ParseFile parses Go source code and returns a ParseResult containing extracted
// nodes, edges, and parse diagnostics.
func (p *GoParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	// Ignore non-Go files or empty source code.
	if filepath.Ext(path) != ".go" {
		return &domain.ParseResult{}, nil
	}
	if len(src) == 0 {
		return &domain.ParseResult{}, nil
	}

	tsParser := goParserPool.Get().(*sitter.Parser)
	defer goParserPool.Put(tsParser)

	tree, err := tsParser.ParseCtx(ctx, nil, src)
	if err != nil {
		// Return parse errors as non-fatal failures to allow processing of other files.
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()
	root := tree.RootNode()

	result := domain.ParseResult{}

	// If tree-sitter reports syntax errors, we cross-check with the standard library
	// go/parser. If the standard library accepts the file, we treat the tree-sitter
	// error as a false positive.
	parseAccepted := true
	if hasErrorNode(root) {
		if pf, ok := goParserCheck(path, src); ok {
			parseAccepted = false
			result.Failures = append(result.Failures, pf)
		}
	}

	// Extract and add the package node.
	pkgNode := buildPackageNode(root, src, repoID, path)
	if pkgNode != nil {
		result.Nodes = append(result.Nodes, pkgNode)
	}

	// Extract the import map and framework commands.
	result.Imports = extractImports(root, src)
	fw := extractFrameworkCommands(root, src, result.Imports, repoID, path)

	// callerCtx records contextual details for a declaration to facilitate call extraction.
	type callerCtx struct {
		caller   *domain.Node
		body     *sitter.Node
		recvName string
		recvType string
		// paramTypes maps parameter names to their declared types to resolve receiver method calls.
		paramTypes map[string]string
	}
	var callers []callerCtx
	symbolByName := map[string]*domain.Node{}

	addSymbol := func(n *domain.Node) {
		result.Nodes = append(result.Nodes, n)
		symbolByName[n.Name] = n
	}

	// Function declarations via the symbols.scm query.
	q, qerr := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "symbols")
	if qerr != nil {
		return nil, qerr
	}
	for _, m := range runQuery(q, root) {
		// Dispatch on the matched AST symbol type to build the appropriate node.
		switch {
		case m.node("function.decl") != nil:
			declNode := m.node("function.decl")
			nameNode := m.node("function.name")
			if nameNode == nil {
				continue
			}
			// Skip declarations within error nodes unless the standard library parser accepted the file.
			if !parseAccepted && hasErrorNode(declNode) {
				continue
			}
			if n := buildFunctionNodeFromCaptures(declNode, nameNode, src, repoID, path); n != nil {
				addSymbol(n)
				callers = append(callers, callerCtx{
					caller:     n,
					body:       m.node("function.body"),
					paramTypes: collectParamReceiverTypes(declNode, src),
				})
			}
		case m.node("method.decl") != nil:
			declNode := m.node("method.decl")
			recvNode := m.node("method.receiver")
			nameNode := m.node("method.name")
			if recvNode == nil || nameNode == nil {
				continue
			}
			if !parseAccepted && hasErrorNode(declNode) {
				continue
			}
			if n := buildMethodNodeFromCaptures(declNode, recvNode, nameNode, src, repoID, path); n != nil {
				addSymbol(n)
				recvName, recvType := extractReceiverBinding(recvNode, src)
				callers = append(callers, callerCtx{
					caller:     n,
					body:       m.node("method.body"),
					recvName:   recvName,
					recvType:   recvType,
					paramTypes: collectParamReceiverTypes(declNode, src),
				})
			}
		case m.node("type.decl") != nil:
			declNode := m.node("type.decl")
			nameNode := m.node("type.name")
			bodyNode := m.node("type.body")
			if nameNode == nil || bodyNode == nil {
				continue
			}
			if !parseAccepted && hasErrorNode(declNode) {
				continue
			}
			if n := buildTypeNodeFromCaptures(declNode, nameNode, bodyNode, src, repoID, path); n != nil {
				addSymbol(n)
				// Surface interface methods as distinct KindMethod nodes so chained-selector
				// calls through interfaces can be resolved.
				if n.Kind == domain.KindInterface {
					for _, im := range parseInterfaceMethods(declNode, src, repoID, path, n.Name) {
						addSymbol(im)
					}
				}
				// Capture embedded types (struct fields / embedded interfaces) so the
				// promoter can materialize EMBEDS edges and promote methods for
				// IMPLEMENTS resolution.
				result.TypeRels = append(result.TypeRels, extractEmbeds(declNode, src, n.ID)...)
			}
		case m.node("var.spec") != nil:
			spec := m.node("var.spec")
			decl := m.node("var.decl")
			if !parseAccepted && hasErrorNode(spec) {
				continue
			}
			for _, n := range buildVarNodesFromSpec(spec, decl, src, repoID, path, domain.KindVariable) {
				if fw.commandVar(n.Name) {
					continue
				}
				addSymbol(n)
			}
		case m.node("const.spec") != nil:
			spec := m.node("const.spec")
			decl := m.node("const.decl")
			if !parseAccepted && hasErrorNode(spec) {
				continue
			}
			for _, n := range buildVarNodesFromSpec(spec, decl, src, repoID, path, domain.KindVariable) {
				addSymbol(n)
			}
		}
	}

	// Register framework command nodes in our symbol map to ensure closure calls inside
	// commands are correctly attributed to the command rather than the package.
	result.Nodes = append(result.Nodes, fw.nodes...)
	maps.Copy(symbolByName, fw.byVar)

	// Build containment edges from the package node to all child symbols.
	if pkgNode != nil {
		for _, n := range result.Nodes {
			if n == pkgNode {
				continue
			}
			e, err := domain.NewEdge(domain.EdgeSpec{
				Src:  pkgNode.ID,
				Tgt:  n.ID,
				Kind: domain.EdgeContains,
			},
				domain.WithConfidence(domain.Definite),
			)
			if err == nil {
				result.Edges = append(result.Edges, e)
			}
		}
	}

	// Extract call edges and unresolved calls by walking the body of each caller node.
	// Field types are scanned to resolve chained selector calls.
	structFields := collectStructFields(root, src)
	// Pre-calculate file-scope variable origins to resolve method calls on package variables.
	pkgVarOrigins := collectPackageVarOrigins(root, src)
	// Collect return types of local constructor functions to resolve method calls on
	// returned values.
	funcReturns := collectInFileFunctionReturns(root, src)
	callsQuery, qerr := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "calls")
	if qerr != nil {
		return nil, qerr
	}
	for _, c := range callers {
		if c.body == nil {
			continue
		}
		callEdges, callUnresolved := extractCallsFromBody(callsQuery, c.body, src, c.caller, c.recvName, c.recvType, symbolByName, structFields, pkgVarOrigins, funcReturns, c.paramTypes)
		result.Edges = append(result.Edges, callEdges...)
		result.UnresolvedCalls = append(result.UnresolvedCalls, callUnresolved...)
	}

	// cobra AddCommand→CONTAINS command-tree edges.
	result.Edges = append(result.Edges, fw.edges...)

	// Append route-to-handler unresolved calls.
	result.UnresolvedCalls = append(result.UnresolvedCalls, fw.unresolved...)

	// Extract calls from anonymous function literals within top-level variable initializations.
	if pkgNode != nil {
		anonEdges, anonUnresolved := extractAnonCallsInTopLevelVars(callsQuery, root, src, symbolByName, pkgNode, pkgVarOrigins, funcReturns)
		result.Edges = append(result.Edges, anonEdges...)
		result.UnresolvedCalls = append(result.UnresolvedCalls, anonUnresolved...)
	}

	// Build chunk nodes for regions of the file not covered by declarations.
	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	// Extract TODO/FIXME comments.
	result.Todos = scanTodos(src)

	return &result, nil
}

// extractCallsFromBody extracts calls from a function or method body using the calls query.
// It returns resolved local calls and unresolved cross-package or cross-file calls.
// extractCallsFromBody has a high parameter count and cyclomatic complexity due to
// matching multiple syntactic call patterns. We accept this complexity to keep
// AST-walking logic self-contained.
//
//nolint:revive,cyclop,funlen // arg-limit (11), cyclomatic (42) and length
func extractCallsFromBody(q *sitter.Query, body *sitter.Node, src []byte, caller *domain.Node, recvName, recvType string, symbols map[string]*domain.Node, structFields map[string]map[string]fieldType, pkgVarOrigins map[string]localVarOrigin, funcReturns map[string]string, paramTypes map[string]string) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := map[string]bool{}
	seenU := map[string]bool{}

	// Map local variable initializations to their source packages (for example,
	// `v := pkg.New()` maps `v` to `pkg`), shadowing file-scoped package variables on collision.
	localOrigins := collectLocalVarOrigins(body, src)
	for name, pkg := range pkgVarOrigins {
		if _, shadowed := localOrigins[name]; shadowed {
			continue
		}
		localOrigins[name] = pkg
	}
	// Map variables initialized from local constructors to their return types to resolve
	// method calls locally.
	localRecvTypes := collectLocalReceiverTypes(body, src, funcReturns)
	// Treat parameters as local receiver type variables, letting local short-variable
	// assignments shadow them.
	for name, typ := range paramTypes {
		if _, shadowed := localRecvTypes[name]; !shadowed {
			localRecvTypes[name] = typ
		}
	}

	for _, m := range runQuery(q, body) {
		var ref callRef
		// Determine the call expression's 1-indexed start line to assign precise locations
		// to call edges.
		var callLine int
		if cn := m.node("call.expr"); cn != nil {
			callLine = int(cn.StartPoint().Row) + 1
		}
		switch {
		case m.node("call.value_arg") != nil && m.node("call.identifier") == nil &&
			m.node("call.operand") == nil && m.node("call.chain_operand") == nil:
			// A bare identifier passed as an argument (for example, `helper(anotherFunc)`)
			// is resolved as a call if it references a local function or method.
			n := m.node("call.value_arg")
			name := string(src[n.StartByte():n.EndByte()])
			callee, ok := symbols[name]
			if !ok || callee == nil {
				continue
			}
			if callee.Kind != domain.KindFunction && callee.Kind != domain.KindMethod {
				continue
			}
			ref = callRef{name: name}
		case m.node("call.identifier") != nil:
			n := m.node("call.identifier")
			ref = callRef{name: string(src[n.StartByte():n.EndByte()])}
		case m.node("call.chain_operand") != nil:
			// Chained selector: outer.inner.Method. Classify based on the leftmost identifier
			// to see if it is a receiver struct field method call or a local variable from an import.
			outerOp := string(src[m.node("call.chain_operand").StartByte():m.node("call.chain_operand").EndByte()])
			innerFld := string(src[m.node("call.chain_field").StartByte():m.node("call.chain_field").EndByte()])
			methodName := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && outerOp == recvName:
				// For s.field.Method, we resolve the method locally if the field's type is defined
				// in this package, otherwise we treat it as an unresolved cross-package method call.
				if fields, ok := structFields[recvType]; ok {
					if ft, ok := fields[innerFld]; ok {
						if ft.pkg == "" {
							ref = callRef{name: ft.name + "." + methodName}
						} else {
							ref = callRef{name: methodName, pkg: ft.pkg, method: true}
						}
					}
				}
			case localOrigins[outerOp] != "":
			}
		case m.node("call.operand") != nil && m.node("call.field") != nil:
			op := string(src[m.node("call.operand").StartByte():m.node("call.operand").EndByte()])
			fld := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && op == recvName:
				// Resolve method calls on the method receiver (for example, `s.Foo()` within a
				// method on `*Server` resolves to `Server.Foo`).
				ref = callRef{name: recvType + "." + fld}
			case localOrigins[op] != "":
				// Resolve method calls on package variables (for example, `v.Method()` where `v := pkg.New()`).
				ref = callRef{name: fld, pkg: localOrigins[op], method: true}
			case localRecvTypes[op] != "":
				// Resolve method calls on variables whose type was inferred from a local constructor
				// function's return type.
				ref = callRef{name: localRecvTypes[op] + "." + fld}
			default:
				// Resolve package-qualified function calls.
				ref = callRef{name: fld, pkg: op}
			}
		default:
			continue
		}

		if ref.name == "" {
			continue
		}
		ref.line = callLine

		if ref.pkg != "" {
			suffix := ""
			if ref.method {
				suffix = "@method"
			}
			key := string(caller.ID) + callKeySep + ref.pkg + "." + ref.name + suffix
			if seenU[key] {
				continue
			}
			seenU[key] = true
			unresolved = append(unresolved, domain.UnresolvedCall{
				CallerID:     caller.ID,
				CalleeName:   ref.name,
				PkgQualifier: ref.pkg,
				IsMethodCall: ref.method,
				SrcLine:      ref.line,
			})
			continue
		}
		callee, ok := symbols[ref.name]
		if !ok {
			key := string(caller.ID) + callKeySep + ref.name
			if seenU[key] {
				continue
			}
			seenU[key] = true
			unresolved = append(unresolved, domain.UnresolvedCall{
				CallerID:   caller.ID,
				CalleeName: ref.name,
				SrcLine:    ref.line,
			})
			continue
		}
		key := string(caller.ID) + callKeySep + string(callee.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		opts := []domain.EdgeOption{domain.WithConfidence(domain.Probable)}
		if ref.line > 0 {
			opts = append(opts, domain.WithSourceLine(ref.line))
		}
		e, err := domain.NewEdge(domain.EdgeSpec{
			Src:  caller.ID,
			Tgt:  callee.ID,
			Kind: domain.EdgeCalls,
		}, opts...)
		if err == nil {
			edges = append(edges, e)
		}
	}
	return edges, unresolved
}

// buildFunctionNodeFromCaptures mirrors parseFunctionDecl in go.go but
// takes the already-located decl + name nodes from a query match. The
// rest (lines, raw content, exported flag, signature) is byte-for-byte
// identical to the legacy path - that is the explicit equivalence
// contract for phase 1.
func buildFunctionNodeFromCaptures(declNode, nameNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	lr := lineRange(declNode)
	// Go allows multiple init functions per file, and multiple blank functions
	// (func _()). Both legitimately share (repo, path, kind, name), so we append
	// a line-number suffix to their node IDs to prevent collisions (one symbol
	// silently dropping the other) while keeping their display names as-is.
	idName := name
	if name == "init" || name == "_" {
		idName = fmt.Sprintf("%s@L%d", name, lr.Start)
	}
	id := nodeID(repoID, path, domain.KindFunction, idName)
	raw := string(src[declNode.StartByte():declNode.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithStructuralHash(domain.ContentHash(goStructuralHash(declNode, src))),
		domain.WithExported(goExported(name)),
	}
	if sig := extractSignature(declNode, src); sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindFunction}, opts...)
	if err != nil {
		return nil
	}
	return n
}

// buildMethodNodeFromCaptures constructs a domain node for a method declaration.
// It uses the receiver type and method name to name the node as 'Receiver.Method'.
func buildMethodNodeFromCaptures(declNode, recvNode, nameNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	receiverType := extractReceiverType(recvNode, src)
	methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
	name := receiverType + "." + methodName
	id := nodeID(repoID, path, domain.KindMethod, name)
	lr := lineRange(declNode)
	raw := string(src[declNode.StartByte():declNode.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithStructuralHash(domain.ContentHash(goStructuralHash(declNode, src))),
		// A method is exported if its method name segment is capitalized.
		domain.WithExported(goExported(methodName)),
	}
	if sig := extractSignature(declNode, src); sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindMethod}, opts...)
	if err != nil {
		return nil
	}
	return n
}

// buildTypeNodeFromCaptures constructs a domain node for a type declaration, mapping
// structs and interfaces to their respective node kinds.
func buildTypeNodeFromCaptures(declNode, nameNode, bodyNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	kind := domain.KindType
	switch bodyNode.Type() {
	case "struct_type":
		kind = domain.KindStruct
	case "interface_type":
		kind = domain.KindInterface
	}
	id := nodeID(repoID, path, kind, name)
	lr := lineRange(declNode)
	raw := string(src[declNode.StartByte():declNode.EndByte()])
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind}, domain.WithLanguage("go"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithStructuralHash(domain.ContentHash(goStructuralHash(declNode, src))), domain.WithExported(goExported(name)))
	if err != nil {
		return nil
	}
	return n
}

// buildVarNodesFromSpec extracts variables declared in a single spec, building one node
// per identifier while skipping blank identifiers. It attributes lines and raw
// content to the enclosing var declaration block.
func buildVarNodesFromSpec(spec, decl *sitter.Node, src []byte, repoID, path string, kind domain.NodeKind) []*domain.Node {
	srcNode := decl
	if srcNode == nil {
		srcNode = spec
	}
	lr := lineRange(srcNode)
	raw := string(src[srcNode.StartByte():srcNode.EndByte()])

	var out []*domain.Node
	named := int(spec.NamedChildCount())
	for i := range named {
		c := spec.NamedChild(i)
		if c == nil || c.Type() != "identifier" {
			continue
		}
		name := string(src[c.StartByte():c.EndByte()])
		if name == "" || name == "_" {
			continue
		}
		id := nodeID(repoID, path, kind, name)
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind}, domain.WithLanguage("go"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(goExported(name)))
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

// extractAnonCallsInTopLevelVars extracts call relationships from anonymous functions
// declared inside top-level variable declarations, attributing the calls directly to
// the declaring variable node if possible.
func extractAnonCallsInTopLevelVars(q *sitter.Query, root *sitter.Node, src []byte, symbols map[string]*domain.Node, pkgNode *domain.Node, pkgVarOrigins map[string]localVarOrigin, funcReturns map[string]string) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seenEdge := map[string]bool{}
	seenU := map[string]bool{}

	// We walk the declaration subtree, tracking the enclosing variable symbol to attribute
	// inner function literal calls directly to it.
	var walk func(n *sitter.Node, caller *domain.Node)
	walk = func(n *sitter.Node, caller *domain.Node) {
		if n == nil {
			return
		}
		if n.Type() == "var_spec" {
			if name := n.ChildByFieldName("name"); name != nil {
				// Only single-variable assignments are mapped to a specific caller symbol;
				// multi-variable declarations default to the package caller.
				id := name
				if id.Type() != "identifier" {
					if int(name.NamedChildCount()) == 1 {
						id = name.NamedChild(0)
					} else {
						id = nil
					}
				}
				if id != nil && id.Type() == "identifier" {
					if v, ok := symbols[string(src[id.StartByte():id.EndByte()])]; ok && v != nil {
						caller = v
					}
				}
			}
		}
		if n.Type() == "func_literal" {
			body := n.ChildByFieldName("body")
			if body != nil {
				e, u := extractCallsFromBody(q, body, src, caller, "", "", symbols, nil, pkgVarOrigins, funcReturns, nil)
				for _, edge := range e {
					key := string(edge.Src) + callKeySep + string(edge.Tgt)
					if seenEdge[key] {
						continue
					}
					seenEdge[key] = true
					edges = append(edges, edge)
				}
				for _, uc := range u {
					suffix := ""
					if uc.IsMethodCall {
						suffix = "@method"
					}
					key := string(uc.CallerID) + callKeySep + uc.PkgQualifier + "." + uc.CalleeName + suffix
					if seenU[key] {
						continue
					}
					seenU[key] = true
					unresolved = append(unresolved, uc)
				}
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i), caller)
		}
	}

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		switch child.Type() {
		case "var_declaration", "const_declaration":
			walk(child, pkgNode)
		}
	}
	return edges, unresolved
}
