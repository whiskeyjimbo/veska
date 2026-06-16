// go_query.go — query-driven Go parser.
// This is the new tree-sitter Query API path. It coexists with go.go
// (the legacy hand-rolled walkers) so an equivalence harness can diff
// the two implementations on the same fixtures before we flip the
// default. Each phase of the rewrite plugs another extractor into this
// file; phase 1 ships only top-level function declarations.
// Construction:
//	parser:= NewGoParser // satisfies ports.CodeParser
//	//. same usage as NewGoParser
// Until equivalence is validated across the entire fixture corpus, the
// daemon's composition root keeps using NewGoParser. Phase 5 flips the
// default and drops the legacy path.

package treesitter

import (
	"context"
	"fmt"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GoParser is the query-driven implementation of ports.CodeParser
// for Go. Construction is cheap (no parser pool yet — phase 1 reuses
// the package-level parserPool from parser_pool.go to keep cgo init
// cost amortised). It is safe for concurrent use.
type GoParser struct{}

// NewGoParser constructs a query-driven Go parser. Until phase 5
// the daemon wires NewGoParser; this constructor exists so the
// equivalence harness and benchmark suite can exercise the new path.
func NewGoParser() *GoParser {
	return &GoParser{}
}

// ParseFile mirrors GoParser.ParseFile's contract: parse src as Go
// source and return a ParseResult of nodes + edges + diagnostics.
// Phase 1 implementation: package node + top-level function
// declarations via queries/go/symbols.scm. Everything else (methods,
// types, calls, imports,.) is produced as empty/nil so the diff
// harness can compare extractor-by-extractor as phases land.
func (p *GoParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	// Non-Go files and empty src return an empty ParseResult and nil
	// error — matches the legacy GoParser contract (go.go ~L44).
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
		// Parse error → surface as a non-fatal Failure, not a Go error.
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()
	root := tree.RootNode()

	result := domain.ParseResult{}

	// /: a syntax error somewhere in the file
	// no longer discards everything. Cross-check with go/parser: if
	// stdlib accepts the file, tree-sitter ERRORs are false positives;
	// otherwise surface go/parser's (more precise) failure.
	parseAccepted := true
	if hasErrorNode(root) {
		if pf, ok := goParserCheck(path, src); ok {
			parseAccepted = false
			result.Failures = append(result.Failures, pf)
		}
	}

	// Package node — the legacy parser emits this; phase 1 keeps parity
	// so the equivalence harness doesn't trip on its absence.
	pkgNode := buildPackageNode(root, src, repoID, path)
	if pkgNode != nil {
		result.Nodes = append(result.Nodes, pkgNode)
	}

	// Import map (also feeds the framework import checks). /
	// qqqy: promote cobra/urfave command struct-literals to KindCommand
	// nodes (the var branch below skips any var promoted here); see
	// go_frameworks.go.
	result.Imports = extractImports(root, src)
	fw := extractFrameworkCommands(root, src, result.Imports, repoID, path)

	// callerCtx records the (callerNode, bodyNode, optional recv binding)
	// triple for each named declaration phase 3 needs to extract calls
	// from. Built during the symbols.scm pass so we don't re-query for
	// declarations later.
	type callerCtx struct {
		caller   *domain.Node
		body     *sitter.Node
		recvName string
		recvType string
		// paramTypes maps the caller's parameter names to their bare
		// same-package type so a method call on a typed parameter resolves
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
			// /: a declaration inside an ERROR
			// subtree has unreliable bytes; skip UNLESS go/parser
			// accepted the file (then ts ERROR nodes are false positives).
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
				// v2: surface each interface method
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
				if fw.commandVar(n.Name) {
					continue // emitted as KindCommand instead
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

	// append KindCommand nodes before the CONTAINS loop so
	// package→symbol edges cover them, and register each under its Go var
	// name in symbolByName — mirroring the KindVariable entry it replaced
	// so the anon-call walker still attributes a command's RunE-closure
	// calls to the command (the cobra grain from ), not the
	// package node.
	result.Nodes = append(result.Nodes, fw.nodes...)
	for varName, n := range fw.byVar {
		symbolByName[varName] = n
	}

	// CONTAINS edges: package → each non-package symbol. Mirrors the
	// loop in go.go just after symbol extraction.
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

	// CALLS edges: for each captured (caller, body), run calls.scm
	// scoped to the body subtree. Each call match becomes an edge
	// (in-file callee found in symbolByName) or an UnresolvedCall
	// (cross-file / cross-package, resolved at promotion).
	// Struct field types are scanned once per file
	// so chained-selector calls `s.field.M` can look up the field's
	// declared type. The map is empty for files with no struct decls.
	structFields := collectStructFields(root, src)
	// /: file-scope `var x = &pkg.Type{.}` or
	// `var x = pkg.New(.)` origins, used by extractCallsFromBody to
	// classify `x.Method` as a cross-package method call. Computed
	// once per file and shared across every function body in the file.
	pkgVarOrigins := collectPackageVarOrigins(root, src)
	// same-file function return types so `v:= New(.);
	// v.Method` in test files (and any other same-package caller)
	// binds Method to ReceiverType.Method via the in-file symbol map.
	// Without this, test functions were invisible to blast/call_chain
	// for in-repo methods because Render lookups dead-ended on a bare
	// "g" qualifier promotion could never resolve.
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

	// gin/echo/chi route→handler references. Emitted as
	// ROUTES UnresolvedCalls so the handler binds against the package-wide
	// symbol map at promotion (same path as a plain cross-file call),
	// materialising a ROUTES edge rather than CALLS.
	result.UnresolvedCalls = append(result.UnresolvedCalls, fw.unresolved...)

	// anonymous-function calls in top-level var/const
	// initialisers. The legacy collectAnonCalls checked node.Type ==
	// "function_literal" but tree-sitter Go's grammar emits
	// "func_literal" — so the cobra-style `var rootCmd = func {. }`
	// CALLS extraction was silently broken all along. Attribute the
	// anon body's calls to the package node so call_chain answers
	// "what does this file initialisation eventually reach" correctly.
	if pkgNode != nil {
		anonEdges, anonUnresolved := extractAnonCallsInTopLevelVars(callsQuery, root, src, symbolByName, pkgNode, pkgVarOrigins, funcReturns)
		result.Edges = append(result.Edges, anonEdges...)
		result.UnresolvedCalls = append(result.UnresolvedCalls, anonUnresolved...)
	}

	// Chunk index over non-declaration regions. Emitted
	// AFTER the symbol set is finalised so chunkFile can carve gaps
	// between symbol line ranges. Mirrors go.go (~L216).
	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	// TODO/FIXME markers via the language-agnostic lexical scanner
	// parity with go.go (~L217). Walks src once for the closed set of
	// marker tokens.
	result.Todos = scanTodos(src)

	return &result, nil
}

// extractCallsFromBody runs calls.scm over a single function/method
// body and classifies each match into either a resolved CALLS edge
// (callee found in symbolByName) or an UnresolvedCall stashed for the
// promotion-time cross-package resolver. recvName/recvType, when
// non-empty, identify the method's receiver so selector calls of the
// form `s.foo` on a known receiver bind to "Receiver.foo" in the
// in-file map (matching parseMethodDecl naming). Dedup is per-caller
// on (callee-name) for unresolved and (caller-id, callee-id) for edges
// to mirror the legacy seen/seenU maps.
// (132 lines) ALL predate — this resolver is one big switch over
// call shapes carrying 10 per-file lookup maps. d521 added paramTypes (11th arg)
// and a small param-merge loop (+2 complexity, +6 lines); the diff-scoped gate
// only re-flags the already-grandfathered function because its signature/body
// changed. Bundling the maps into a context struct / splitting the switch is a
// separate refactor, out of scope for this bugfix.
//
//nolint:revive,cyclop,funlen // arg-limit (11), cyclomatic (42) and length
func extractCallsFromBody(q *sitter.Query, body *sitter.Node, src []byte, caller *domain.Node, recvName, recvType string, symbols map[string]*domain.Node, structFields map[string]map[string]fieldType, pkgVarOrigins map[string]localVarOrigin, funcReturns map[string]string, paramTypes map[string]string) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := map[string]bool{}
	seenU := map[string]bool{}

	// Per-body local-var origins: `v:= pkg.New(.)`
	// produces v→pkg so subsequent `v.X` chained-selector calls are
	// recognised as method calls on a value from pkg. File-scope `var x =
	// &pkg.Type{.}` origins feed the same
	// lookup; function-body short-var origins shadow them on collision,
	// matching Go's scoping rules.
	localOrigins := collectLocalVarOrigins(body, src)
	for name, pkg := range pkgVarOrigins {
		if _, shadowed := localOrigins[name]; shadowed {
			continue
		}
		localOrigins[name] = pkg
	}
	// per-body local-var receiver-type origins (v:= New
	// where New is a same-file function returning a same-package type).
	// Kept distinct from localOrigins/pkgVarOrigins because the
	// downstream lookup binds against the in-file symbol map directly,
	// not against a cross-repo stub.
	localRecvTypes := collectLocalReceiverTypes(body, src, funcReturns)
	// A typed parameter is a valid method-call receiver too:
	// fold the caller's param→type map in as a base, but let body-local
	// short-var origins SHADOW a param of the same name (Go scoping).
	for name, typ := range paramTypes {
		if _, shadowed := localRecvTypes[name]; !shadowed {
			localRecvTypes[name] = typ
		}
	}

	for _, m := range runQuery(q, body) {
		var ref callRef
		// Capture the call_expression's 1-indexed start line so resolved
		// edges and UnresolvedCalls carry the actual call-site location,
		// not the caller node's declaration line.
		// tree-sitter Row is 0-indexed; add 1. Missing @call.expr (e.g. a
		// future calls.scm pattern that omits the wrapper capture) leaves
		// line at 0 — the renderer falls back to the caller's line.
		var callLine int
		if cn := m.node("call.expr"); cn != nil {
			callLine = int(cn.StartPoint().Row) + 1
		}
		switch {
		case m.node("call.value_arg") != nil && m.node("call.identifier") == nil &&
			m.node("call.operand") == nil && m.node("call.chain_operand") == nil:
			// bare identifier passed as a call argument
			// (helper(boolConv)). Treat as a CALLS edge to boolConv when
			// the identifier resolves to a same-file function/method
			// otherwise it's a local variable or parameter and we skip.
			// The symbol-map check filters here rather than later because
			// the regular call branches below all flow through unresolved
			// emission if the symbol misses, which would generate noise
			// for every variable passed as an argument.
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
			// Chained selector: outer.inner.Method. Classify based on
			// the inner operand (the leftmost identifier) — either it's
			// the method receiver (struct field method call, phase E)
			// or a local variable assigned from pkg.X (phase A).
			outerOp := string(src[m.node("call.chain_operand").StartByte():m.node("call.chain_operand").EndByte()])
			innerFld := string(src[m.node("call.chain_field").StartByte():m.node("call.chain_field").EndByte()])
			methodName := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && outerOp == recvName:
				// `s.field.Method` — phase E. Look up the field on
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
				// `v.M` follows-through to `local.X.M` only when
				// the legacy parser also drops it — neither path
				// captures the chained variant of local-var origins.
				// We do nothing here so phase 3b stays equivalent.
			}
		case m.node("call.operand") != nil && m.node("call.field") != nil:
			op := string(src[m.node("call.operand").StartByte():m.node("call.operand").EndByte()])
			fld := string(src[m.node("call.field").StartByte():m.node("call.field").EndByte()])
			switch {
			case recvName != "" && recvType != "" && op == recvName:
				// s.foo inside a method on *Server -> Server.foo (local).
				ref = callRef{name: recvType + "." + fld}
			case localOrigins[op] != "":
				// v.Method where v:= pkg.New(.) — phase A.
				ref = callRef{name: fld, pkg: localOrigins[op], method: true}
			case localRecvTypes[op] != "":
				// v.Method where v:= SamePkgFunc(.)
				// and SamePkgFunc returns a same-package type T → bind
				// to T.Method via the in-file symbol map. Same shape as
				// the recvName branch above, just with the receiver
				// type inferred from a local constructor call rather
				// than a method declaration's receiver.
				ref = callRef{name: localRecvTypes[op] + "." + fld}
			default:
				// pkg.Foo — package-qualified.
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
// identical to the legacy path — that is the explicit equivalence
// contract for phase 1.
func buildFunctionNodeFromCaptures(declNode, nameNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	lr := lineRange(declNode)
	// Go allows multiple `func init` per file. Without a per-decl
	// discriminator they all hash to the same node_id and the promotion
	// transaction fails with a UNIQUE-PK constraint on (node_id, branch)
	// observed on hugo and prometheus, where protobuf-generated.pb.go
	// files routinely declare two init. The display name
	// stays "init"; only the ID input is disambiguated.
	idName := name
	if name == "init" {
		idName = fmt.Sprintf("init@L%d", lr.Start)
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
		domain.WithStructuralHash(domain.ContentHash(goStructuralHash(declNode, src))),
		// Method is exported when the method name (after "Receiver.")
		// is capitalised; the receiver's casing is irrelevant.
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
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind}, domain.WithLanguage("go"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithStructuralHash(domain.ContentHash(goStructuralHash(declNode, src))), domain.WithExported(goExported(name)))
	if err != nil {
		return nil
	}
	return n
}

// buildVarNodesFromSpec mirrors parseTopLevelVarSpec: one node per
// declared identifier, skipping anonymous "_" names. Multiple names
// sharing one spec (`var a, b = 1, 2`) yield two nodes. Lines + raw
// content come from the ENCLOSING declaration (not the spec) so a
// grouped var (. ) block embeds the whole block in raw_content
// the legacy parser does this so semantic search indexes cobra-style
// struct-literal initialisers (go.go ~L445). When decl is nil (the
// pattern omitted the @decl capture) we fall back to the spec.
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

// extractAnonCallsInTopLevelVars walks every top-level var_declaration
// and const_declaration for func_literal subtrees, then runs the
// existing calls.scm extractor against each literal's body. When the
// surrounding var has a name we can resolve to an extracted Variable
// node (e.g. `var helloCmd = &cobra.Command{ RunE: func{ Foo } }`),
// the calls attribute to that var node so cross-repo blast can name
// the actual caller (`helloCmd`) instead of the package node — closing
// the cobra-app grain gap. Falls back to pkgNode for
// shapes where no enclosing var is identifiable (const blocks,
// composite-literal blanket lookups). Dedup is per-(caller, target)
// across all anon bodies so two literals calling the same target
// produce one edge per caller.
func extractAnonCallsInTopLevelVars(q *sitter.Query, root *sitter.Node, src []byte, symbols map[string]*domain.Node, pkgNode *domain.Node, pkgVarOrigins map[string]localVarOrigin, funcReturns map[string]string) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seenEdge := map[string]bool{}
	seenU := map[string]bool{}

	// walk threads the currently-enclosing var node (if any) through
	// the descent. When a var_spec with a single name appears we look
	// it up in symbols by bare name and shadow caller for the subtree.
	// Hitting another var_spec replaces it; leaving the subtree restores
	// the caller via the deferred restore.
	var walk func(n *sitter.Node, caller *domain.Node)
	walk = func(n *sitter.Node, caller *domain.Node) {
		if n == nil {
			return
		}
		if n.Type() == "var_spec" {
			if name := n.ChildByFieldName("name"); name != nil {
				// var_spec.name is either an identifier directly
				// (`var x =.`) or a comma-separated list whose
				// children are identifiers (`var x, y =.`).
				// Only single-name shapes can attribute calls
				// unambiguously; multi-name var blocks fall through
				// to pkgNode.
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
