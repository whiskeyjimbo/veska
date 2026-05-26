// go_query.go — query-driven Go parser (solov2-1yev phase 1).
//
// This is the new tree-sitter Query API path. It coexists with go.go
// (the legacy hand-rolled walkers) so an equivalence harness can diff
// the two implementations on the same fixtures before we flip the
// default. Each phase of the rewrite plugs another extractor into this
// file; phase 1 ships only top-level function declarations.
//
// Construction:
//
//	parser := NewGoQueryParser()   // satisfies ports.CodeParser
//	// ... same usage as NewGoParser()
//
// Until equivalence is validated across the entire fixture corpus, the
// daemon's composition root keeps using NewGoParser. Phase 5 flips the
// default and drops the legacy path.
package treesitter

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GoQueryParser is the query-driven implementation of ports.CodeParser
// for Go. Construction is cheap (no parser pool yet — phase 1 reuses
// the package-level parserPool from parser_pool.go to keep cgo init
// cost amortised). It is safe for concurrent use.
type GoQueryParser struct{}

// NewGoQueryParser constructs a query-driven Go parser. Until phase 5
// the daemon wires NewGoParser; this constructor exists so the
// equivalence harness and benchmark suite can exercise the new path.
func NewGoQueryParser() *GoQueryParser {
	return &GoQueryParser{}
}

// ParseFile mirrors GoParser.ParseFile's contract: parse src as Go
// source and return a ParseResult of nodes + edges + diagnostics.
// Phase 1 implementation: package node + top-level function
// declarations via queries/go/symbols.scm. Everything else (methods,
// types, calls, imports, ...) is produced as empty/nil so the diff
// harness can compare extractor-by-extractor as phases land.
func (p *GoQueryParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (domain.ParseResult, error) {
	tsParser := goParserPool.Get().(*sitter.Parser)
	defer goParserPool.Put(tsParser)

	tree, err := tsParser.ParseCtx(ctx, nil, src)
	if err != nil {
		return domain.ParseResult{}, fmt.Errorf("go_query: parse %s: %w", path, err)
	}
	defer tree.Close()
	root := tree.RootNode()

	// solov2-0kv6 mirror: when go/parser accepts the file, tree-sitter's
	// ERROR nodes are false positives. Track parseAccepted so the
	// per-decl skip can re-include declarations the recursive walker
	// would have accepted.
	parseAccepted := true
	if _, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution); perr != nil {
		parseAccepted = false
	}

	result := domain.ParseResult{}

	// Package node — the legacy parser emits this; phase 1 keeps parity
	// so the equivalence harness doesn't trip on its absence.
	pkgNode := buildPackageNode(root, src, repoID, path)
	if pkgNode != nil {
		result.Nodes = append(result.Nodes, pkgNode)
	}

	// callerCtx records the (callerNode, bodyNode, optional recv binding)
	// triple for each named declaration phase 3 needs to extract calls
	// from. Built during the symbols.scm pass so we don't re-query for
	// declarations later.
	type callerCtx struct {
		caller   *domain.Node
		body     *sitter.Node
		recvName string
		recvType string
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
		return domain.ParseResult{}, qerr
	}
	for _, m := range runQuery(q, root) {
		// One pattern from symbols.scm fires per match; dispatch on the
		// capture name set. Each branch mirrors the corresponding
		// legacy parser function (parseFunctionDecl, parseMethodDecl,
		// parseTypeDecl, parseTopLevelVarDecl) one-for-one.
		switch {
		case m.node("function.decl") != nil:
			declNode := m.node("function.decl")
			nameNode := m.node("function.name")
			if nameNode == nil {
				continue
			}
			// solov2-7nkm / solov2-0kv6: a declaration inside an ERROR
			// subtree has unreliable bytes; skip UNLESS go/parser
			// accepted the file (then ts ERROR nodes are false positives).
			if !parseAccepted && hasErrorNode(declNode) {
				continue
			}
			if n := buildFunctionNodeFromCaptures(declNode, nameNode, src, repoID, path); n != nil {
				addSymbol(n)
				callers = append(callers, callerCtx{caller: n, body: m.node("function.body")})
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
					caller:   n,
					body:     m.node("method.body"),
					recvName: recvName,
					recvType: recvType,
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
				// solov2-9rc2 phase E v2: surface each interface method
				// as its own KindMethod node so chained-selector calls
				// through interface fields resolve at promotion time.
				if n.Kind == domain.KindInterface {
					for _, im := range parseInterfaceMethods(declNode, src, repoID, path, n.Name) {
						addSymbol(im)
					}
				}
			}
		case m.node("var.spec") != nil:
			spec := m.node("var.spec")
			decl := m.node("var.decl")
			if !parseAccepted && hasErrorNode(spec) {
				continue
			}
			for _, n := range buildVarNodesFromSpec(spec, decl, src, repoID, path, domain.KindVariable) {
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

	// CONTAINS edges: package → each non-package symbol. Mirrors the
	// loop in go.go just after symbol extraction.
	if pkgNode != nil {
		for _, n := range result.Nodes {
			if n == pkgNode {
				continue
			}
			e, err := domain.NewEdge(pkgNode.ID, n.ID, domain.EdgeContains,
				domain.WithConfidence(domain.Definite),
			)
			if err == nil {
				result.Edges = append(result.Edges, e)
			}
		}
	}

	// CALLS edges: for each captured (caller, body), run calls.scm
	// scoped to the body subtree. Each call match becomes an edge
	// (in-file callee found in symbolByName) or an UnresolvedCall
	// (cross-file / cross-package, resolved at promotion).
	//
	// Struct field types are scanned once per file (solov2-9rc2 phase E)
	// so chained-selector calls `s.field.M()` can look up the field's
	// declared type. The map is empty for files with no struct decls.
	structFields := collectStructFields(root, src)
	callsQuery, qerr := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "calls")
	if qerr != nil {
		return domain.ParseResult{}, qerr
	}
	for _, c := range callers {
		if c.body == nil {
			continue
		}
		callEdges, callUnresolved := extractCallsFromBody(callsQuery, c.body, src, c.caller, c.recvName, c.recvType, symbolByName, structFields)
		result.Edges = append(result.Edges, callEdges...)
		result.UnresolvedCalls = append(result.UnresolvedCalls, callUnresolved...)
	}

	// Import map. extractImports already exists and is the same shape
	// the legacy parser emits, so reuse it directly.
	result.Imports = extractImports(root, src)

	return result, nil
}

// extractCallsFromBody runs calls.scm over a single function/method
// body and classifies each match into either a resolved CALLS edge
// (callee found in symbolByName) or an UnresolvedCall stashed for the
// promotion-time cross-package resolver. recvName/recvType, when
// non-empty, identify the method's receiver so selector calls of the
// form `s.foo()` on a known receiver bind to "Receiver.foo" in the
// in-file map (matching parseMethodDecl naming). Dedup is per-caller
// on (callee-name) for unresolved and (caller-id, callee-id) for edges
// to mirror the legacy seen/seenU maps.
func extractCallsFromBody(q *sitter.Query, body *sitter.Node, src []byte, caller *domain.Node, recvName, recvType string, symbols map[string]*domain.Node, structFields map[string]map[string]fieldType) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := map[string]bool{}
	seenU := map[string]bool{}

	// Per-body local-var origins (solov2-9rc2 phase A): `v := pkg.New(...)`
	// produces v→pkg so subsequent `v.X()` chained-selector calls are
	// recognised as method calls on a value from pkg.
	localOrigins := collectLocalVarOrigins(body, src)

	for _, m := range runQuery(q, body) {
		var ref callRef
		switch {
		case m.node("call.identifier") != nil:
			n := m.node("call.identifier")
			ref = callRef{name: string(src[n.StartByte():n.EndByte()])}
		case m.node("call.chain_operand") != nil:
			// Chained selector: outer.inner.Method(). Classify based on
			// the inner operand (the leftmost identifier) — either it's
			// the method receiver (struct field method call, phase E)
			// or a local variable assigned from pkg.X (phase A).
			outerOp := string(src[m.node("call.chain_operand").StartByte():m.node("call.chain_operand").EndByte()])
			innerFld := string(src[m.node("call.chain_field").StartByte():m.node("call.chain_field").EndByte()])
			methodName := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && outerOp == recvName:
				// `s.field.Method()` — phase E. Look up the field on
				// the receiver type. Same-package concrete struct
				// resolves to FieldType.Method via the in-file symbol
				// map; cross-package emits IsMethodCall=true.
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
				// `v.M()` follows-through to `local.X.M()` only when
				// the legacy parser also drops it — neither path
				// captures the chained variant of local-var origins.
				// We do nothing here so phase 3b stays equivalent.
			}
		case m.node("call.operand") != nil && m.node("call.field") != nil:
			op := string(src[m.node("call.operand").StartByte():m.node("call.operand").EndByte()])
			fld := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && op == recvName:
				// s.foo() inside a method on *Server -> Server.foo (local).
				ref = callRef{name: recvType + "." + fld}
			case localOrigins[op] != "":
				// v.Method() where v := pkg.New(...) — phase A.
				ref = callRef{name: fld, pkg: localOrigins[op], method: true}
			default:
				// pkg.Foo() — package-qualified.
				ref = callRef{name: fld, pkg: op}
			}
		default:
			continue
		}

		// An unset ref (empty name) means classification skipped — the
		// chained-selector branch dropped a shape phase 3b doesn't
		// handle yet.
		if ref.name == "" {
			continue
		}

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
			})
			continue
		}
		key := string(caller.ID) + callKeySep + string(callee.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		e, err := domain.NewEdge(caller.ID, callee.ID, domain.EdgeCalls,
			domain.WithConfidence(domain.Probable),
		)
		if err == nil {
			edges = append(edges, e)
		}
	}
	return edges, unresolved
}

// buildFunctionNodeFromCaptures mirrors parseFunctionDecl in go.go but
// takes the already-located decl + name nodes from a query match. The
// rest (lines, raw content, exported flag, signature) is byte-for-byte
// identical to the legacy path — that is the explicit equivalence
// contract for phase 1.
func buildFunctionNodeFromCaptures(declNode, nameNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(declNode)
	raw := string(src[declNode.StartByte():declNode.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(name)),
	}
	if sig := extractSignature(declNode, src); sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}
	n, err := domain.NewNode(id, path, name, domain.KindFunction, opts...)
	if err != nil {
		return nil
	}
	return n
}

// buildMethodNodeFromCaptures mirrors parseMethodDecl in go.go. The
// query captured the method_declaration, its receiver parameter_list,
// and its name (field_identifier); we reuse extractReceiverType to
// strip pointer/value/etc. and build the canonical "Receiver.Method"
// node name.
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
		// Method is exported when the method name (after "Receiver.")
		// is capitalised; the receiver's casing is irrelevant.
		domain.WithExported(goExported(methodName)),
	}
	if sig := extractSignature(declNode, src); sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}
	n, err := domain.NewNode(id, path, name, domain.KindMethod, opts...)
	if err != nil {
		return nil
	}
	return n
}

// buildTypeNodeFromCaptures mirrors parseTypeDecl: dispatches between
// KindStruct / KindInterface / KindType based on the type_spec's body
// node type. The captured @type.decl is the whole type_declaration
// (legacy uses it as the lineRange / raw_content source); @type.name
// gives the identifier; @type.body is the struct_type / interface_type
// / other.
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
	n, err := domain.NewNode(id, path, name, kind,
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(name)),
	)
	if err != nil {
		return nil
	}
	return n
}

// buildVarNodesFromSpec mirrors parseTopLevelVarSpec: one node per
// declared identifier, skipping anonymous "_" names. Multiple names
// sharing one spec (`var a, b = 1, 2`) yield two nodes. Lines + raw
// content come from the ENCLOSING declaration (not the spec) so a
// grouped var ( ... ) block embeds the whole block in raw_content —
// the legacy parser does this so semantic search indexes cobra-style
// struct-literal initialisers (go.go ~L445). When decl is nil (the
// pattern omitted the @decl capture) we fall back to the spec.
func buildVarNodesFromSpec(spec, decl *sitter.Node, src []byte, repoID, path string, kind domain.NodeKind) []*domain.Node {
	src_node := decl
	if src_node == nil {
		src_node = spec
	}
	lr := lineRange(src_node)
	raw := string(src[src_node.StartByte():src_node.EndByte()])

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
		n, err := domain.NewNode(id, path, name, kind,
			domain.WithLanguage("go"),
			domain.WithLines(lr),
			domain.WithRawContent(raw),
			domain.WithExported(goExported(name)),
		)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

// buildPackageNode produces the package-clause node the legacy parser
// emits at the top of every Go file. Extracted into a helper here so
// the query parser keeps parity until packages get their own .scm
// (phase 2 candidate).
func buildPackageNode(root *sitter.Node, src []byte, repoID, path string) *domain.Node {
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child.Type() != "package_clause" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			// Some Go grammar versions expose the identifier as a plain
			// named child rather than via the "name" field. Walk for it.
			named := int(child.NamedChildCount())
			for j := range named {
				c := child.NamedChild(j)
				if c != nil && c.Type() == "package_identifier" {
					nameNode = c
					break
				}
			}
		}
		if nameNode == nil {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		id := nodeID(repoID, path, domain.KindPackage, name)
		// Legacy parser intentionally omits Lines on the package node —
		// extractPackageName + NewNode-without-WithLines (go.go ~L92).
		// Match that exactly so the equivalence harness stays green.
		n, err := domain.NewNode(id, path, name, domain.KindPackage,
			domain.WithLanguage("go"),
		)
		if err != nil {
			return nil
		}
		return n
	}
	return nil
}
