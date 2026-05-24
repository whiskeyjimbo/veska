package treesitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// goExported reports whether a Go identifier is exported — its first rune is an
// uppercase letter. Names like "Receiver.Method" should be passed as the bare
// method-name segment by the caller.
func goExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

// GoParser is a tree-sitter-backed implementation of ports.CodeParser for Go source files.
// Each ParseFile call borrows a parser from goParserPool (solov2-0ung), so the
// per-language CGO setup amortises across the watcher's re-parse churn and
// bulk re-index passes.
type GoParser struct{}

// NewGoParser returns a new GoParser.
func NewGoParser() *GoParser {
	return &GoParser{}
}

// ParseFile parses the Go source in src and returns the Nodes and Edges extracted from it.
// Non-Go files (by extension) return an empty ParseResult and nil error.
// If the tree-sitter parse produces error nodes the result is empty (parse errors are non-fatal).
func (p *GoParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	if filepath.Ext(path) != ".go" {
		return &domain.ParseResult{}, nil
	}
	if len(src) == 0 {
		return &domain.ParseResult{}, nil
	}

	parser := goParserPool.Get().(*sitter.Parser)
	defer goParserPool.Put(parser)

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	result := &domain.ParseResult{}

	// Error recovery (solov2-7nkm): a syntax error somewhere in the file no
	// longer discards the whole file. Surface the parse failure, but still
	// extract every well-formed top-level declaration — per-declaration error
	// subtrees are skipped below so a function broken mid-edit doesn't erase
	// its siblings (the watcher re-parses on save, exactly when files are
	// transiently broken).
	if hasErrorNode(root) {
		result.Failures = append(result.Failures, firstErrorFailure(root))
	}

	// --- package node ---
	pkgName := extractPackageName(root, src)
	var pkgNode *domain.Node
	if pkgName != "" {
		id := nodeID(repoID, path, domain.KindPackage, pkgName)
		n, err := domain.NewNode(id, path, pkgName, domain.KindPackage,
			domain.WithLanguage("go"),
		)
		if err == nil {
			pkgNode = n
			result.Nodes = append(result.Nodes, pkgNode)
		}
	}

	// --- symbol nodes indexed by name for CALLS resolution ---
	symbolByName := map[string]*domain.Node{}

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		// Skip declarations whose own subtree contains a syntax error — their
		// extracted name/signature/body would be unreliable. Sibling
		// declarations that parsed cleanly are still indexed (solov2-7nkm).
		if hasErrorNode(child) {
			continue
		}
		switch child.Type() {
		case "function_declaration":
			n := parseFunctionDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		case "method_declaration":
			n := parseMethodDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		case "type_declaration":
			n := parseTypeDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		case "var_declaration", "const_declaration":
			// solov2-b7wt: extract top-level (package-scope) var/const
			// names so framework-config patterns where the API surface
			// lives in initialised vars — cobra command trees, gin/echo
			// router globals, viper config singletons — are discoverable
			// via eng_find_symbol / eng_get_file_nodes.
			for _, n := range parseTopLevelVarDecl(child, src, repoID, path) {
				result.Nodes = append(result.Nodes, n)
				if _, exists := symbolByName[n.Name]; !exists {
					symbolByName[n.Name] = n
				}
			}
		}
	}

	// --- CONTAINS edges: package -> each symbol ---
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

	// --- CALLS edges ---
	// Resolved-locally calls become edges; calls naming a symbol that's
	// not in this file's map (likely another file in the same Go package)
	// surface as UnresolvedCalls and get bound at promotion time
	// (solov2-2at).
	callEdges, unresolved := extractCallEdges(root, src, symbolByName)
	result.Edges = append(result.Edges, callEdges...)
	result.UnresolvedCalls = unresolved

	// Anonymous functions used as values in top-level var/const declarations
	// (the cobra `var serveCmd = &cobra.Command{Run: func() { Serve() }}`
	// pattern) have no enclosing named function for extractCallEdges to
	// attach calls to — without this pass they were invisible to call_chain,
	// which is the dominant call pattern in any cobra-based CLI (solov2-kzxh).
	// Attribute their calls to the package node so call_chain answers
	// "what eventually gets reached when this file initialises" correctly.
	if pkgNode != nil {
		varEdges, varUnresolved := extractTopLevelVarInitCalls(root, src, symbolByName, pkgNode)
		result.Edges = append(result.Edges, varEdges...)
		result.UnresolvedCalls = append(result.UnresolvedCalls, varUnresolved...)
	}

	// --- import map for cross-package call resolution (solov2-xc51) ---
	result.Imports = extractImports(root, src)

	// --- chunk index over non-declaration regions (solov2-jyt) ---
	// Emitted AFTER CALLS so the symbol set used for CALLS resolution
	// stays purely declarative — chunks aren't callable symbols.
	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	// --- TODO/FIXME markers (language-agnostic lexical scan) ---
	result.Todos = scanTodos(src)

	return result, nil
}

// ----- node extraction helpers -----

func parseFunctionDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])

	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(name)),
	}

	sig := extractSignature(node, src)
	if sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}

	n, err := domain.NewNode(id, path, name, domain.KindFunction, opts...)
	if err != nil {
		return nil
	}
	return n
}

func parseMethodDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	// receiver field contains the parameter_list with the receiver spec
	receiverNode := node.ChildByFieldName("receiver")
	nameNode := node.ChildByFieldName("name")
	if receiverNode == nil || nameNode == nil {
		return nil
	}

	receiverType := extractReceiverType(receiverNode, src)
	methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
	name := receiverType + "." + methodName

	id := nodeID(repoID, path, domain.KindMethod, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		// A method is exported when its method name (after "Receiver.") is
		// capitalised; the receiver type's casing is irrelevant.
		domain.WithExported(goExported(methodName)),
	}

	sig := extractSignature(node, src)
	if sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}

	n, err := domain.NewNode(id, path, name, domain.KindMethod, opts...)
	if err != nil {
		return nil
	}
	return n
}

func parseTypeDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	// type_declaration -> type_spec -> name + type
	count := int(node.ChildCount())
	for i := range count {
		spec := node.Child(i)
		if spec.Type() != "type_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		typeNode := spec.ChildByFieldName("type")
		if nameNode == nil || typeNode == nil {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])

		kind := domain.KindType
		switch typeNode.Type() {
		case "struct_type":
			kind = domain.KindStruct
		case "interface_type":
			kind = domain.KindInterface
		}

		id := nodeID(repoID, path, kind, name)
		lr := lineRange(node)
		raw := string(src[node.StartByte():node.EndByte()])

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
	return nil
}

// parseTopLevelVarDecl extracts every name declared by a top-level
// var_declaration or const_declaration as a KindVariable node. Tree-sitter's
// grammar nests one or more var_spec / const_spec children inside the
// declaration; each spec may itself bind multiple identifiers
// (`var a, b = 1, 2`). Underscore names (`_`) are skipped — they aren't
// addressable.
//
// Captured for framework-config patterns where the API surface lives in
// initialised package-scope vars: cobra `var rootCmd = &cobra.Command{...}`,
// gin/echo router globals, viper config singletons. Without this pass the
// graph misses the entire command tree of any cobra-based CLI and
// eng_find_symbol returns empty for the var names users actually search
// for (solov2-b7wt).
func parseTopLevelVarDecl(node *sitter.Node, src []byte, repoID, path string) []*domain.Node {
	var out []*domain.Node
	specKind := "var_spec"
	if node.Type() == "const_declaration" {
		specKind = "const_spec"
	}
	// tree-sitter Go represents a `var ( ... )` block either as a
	// var_declaration whose direct children are var_specs OR (in newer
	// grammar versions) as a var_declaration -> var_spec_list -> var_spec.
	// Walk both shapes to stay grammar-version-tolerant.
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		for i := range int(n.ChildCount()) {
			c := n.Child(i)
			switch c.Type() {
			case specKind:
				parseTopLevelVarSpec(c, src, repoID, path, node, &out)
			case "var_spec_list", "const_spec_list":
				visit(c)
			}
		}
	}
	visit(node)
	return out
}

func parseTopLevelVarSpec(spec *sitter.Node, src []byte, repoID, path string, decl *sitter.Node, out *[]*domain.Node) {
	for j := range int(spec.ChildCount()) {
		nameNode := spec.Child(j)
		if nameNode.Type() != "identifier" {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		if name == "" || name == "_" {
			continue
		}
		id := nodeID(repoID, path, domain.KindVariable, name)
		lr := lineRange(decl)
		// Capture the whole declaration text — including any struct
		// literal initialiser — so eng_search_semantic indexes the
		// cobra Use:/Short:/Long: strings that describe the command.
		raw := string(src[decl.StartByte():decl.EndByte()])
		n, err := domain.NewNode(id, path, name, domain.KindVariable,
			domain.WithLanguage("go"),
			domain.WithLines(lr),
			domain.WithRawContent(raw),
			domain.WithExported(goExported(name)),
		)
		if err != nil {
			continue
		}
		*out = append(*out, n)
	}
}

// ----- CALLS extraction -----

// callKeySep separates the parts of an in-file call-dedup key. A NUL byte
// cannot appear in a node id or identifier, so it is unambiguous and shared by
// both the resolved-edge (seen) and unresolved-call (seenU) maps (solov2-2efk).
const callKeySep = "\x00"

// Cross-package call handling (solov2-xc51): collectCallNames returns
// package-qualified calls (pkg.Bar()) with callRef.pkg set. extractCallEdges
// cannot bind them in-file, so it stashes them as UnresolvedCalls carrying the
// qualifier; the promotion store resolves each against the file's import map —
// to a concrete CALLS edge for intra-module targets, or a cross-repo edge stub
// for external modules (which the query-time resolver later binds, solov2-1gj).

// extractCallEdges walks the entire AST looking for call_expression nodes inside
// function/method bodies and emits EdgeCalls when the callee is known in the file.
func extractCallEdges(root *sitter.Node, src []byte, symbols map[string]*domain.Node) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := make(map[string]bool) // dedupe same caller→callee within a file
	seenU := make(map[string]bool)

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		var callerNode *domain.Node
		var recvName, recvType string

		switch child.Type() {
		case "function_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			callerNode = symbols[string(src[nameNode.StartByte():nameNode.EndByte()])]
		case "method_declaration":
			receiverNode := child.ChildByFieldName("receiver")
			nameNode := child.ChildByFieldName("name")
			if receiverNode == nil || nameNode == nil {
				continue
			}
			recvName, recvType = extractReceiverBinding(receiverNode, src)
			name := recvType + "." + string(src[nameNode.StartByte():nameNode.EndByte()])
			callerNode = symbols[name]
		default:
			continue
		}

		if callerNode == nil {
			continue
		}

		bodyNode := child.ChildByFieldName("body")
		if bodyNode == nil {
			continue
		}

		// Identifier calls (foo()) resolve directly against the file's symbol
		// map; receiver selector calls (s.foo() in a method on *Server) are
		// rewritten as Server.foo and resolved too (solov2-q9p). Package-
		// qualified calls (pkg.Bar) cannot bind in-file — they are stashed as
		// UnresolvedCalls for promotion-time resolution (see the cross-package
		// note above).
		callRefs := collectCallNames(bodyNode, src, recvName, recvType)
		for _, ref := range callRefs {
			if ref.pkg != "" {
				key := string(callerNode.ID) + callKeySep + ref.pkg + "." + ref.name
				if seenU[key] {
					continue
				}
				seenU[key] = true
				unresolved = append(unresolved, domain.UnresolvedCall{
					CallerID:     callerNode.ID,
					CalleeName:   ref.name,
					PkgQualifier: ref.pkg,
				})
				continue
			}
			calleeNode, ok := symbols[ref.name]
			if !ok {
				// Stash for cross-file (same-package) resolution at promotion
				// time (solov2-2at). Dedupe per (caller, callee-name) so
				// repeated call sites yield one resolution attempt.
				key := string(callerNode.ID) + callKeySep + ref.name
				if seenU[key] {
					continue
				}
				seenU[key] = true
				unresolved = append(unresolved, domain.UnresolvedCall{
					CallerID:   callerNode.ID,
					CalleeName: ref.name,
				})
				continue
			}
			key := string(callerNode.ID) + callKeySep + string(calleeNode.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			e, err := domain.NewEdge(callerNode.ID, calleeNode.ID, domain.EdgeCalls,
				domain.WithConfidence(domain.Probable),
			)
			if err == nil {
				edges = append(edges, e)
			}
		}
	}
	return edges, unresolved
}

// extractTopLevelVarInitCalls walks top-level var_declaration and const_declaration
// children, finds function_literal bodies anywhere inside them, and emits CALLS
// edges from pkgNode (the file's package node) to every callable target in those
// bodies. This makes cobra-style anonymous-function call patterns visible to
// call_chain and blast_radius (solov2-kzxh).
//
// Only identifier-form calls are bound here; package-qualified and selector
// calls follow the same paths as extractCallEdges (UnresolvedCalls for
// cross-package, in-file symbol map for selectors on a known receiver).
func extractTopLevelVarInitCalls(root *sitter.Node, src []byte, symbols map[string]*domain.Node, pkgNode *domain.Node) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := make(map[string]bool)
	seenU := make(map[string]bool)

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		switch child.Type() {
		case "var_declaration", "const_declaration":
			// Find every function_literal anywhere inside this declaration's
			// subtree and collect calls from each body.
			collectAnonCalls(child, src, symbols, pkgNode, &edges, &unresolved, seen, seenU)
		}
	}
	return edges, unresolved
}

// collectAnonCalls walks node looking for function_literal subtrees; for each
// one it harvests identifier and package-qualified calls in the body and
// attributes them to callerNode. Recursive so nested closures
// (func(){ go func(){ Foo() }() }) are reached too.
func collectAnonCalls(node *sitter.Node, src []byte, symbols map[string]*domain.Node, callerNode *domain.Node, edges *[]*domain.Edge, unresolved *[]domain.UnresolvedCall, seen, seenU map[string]bool) {
	if node == nil {
		return
	}
	if node.Type() == "function_literal" {
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil {
			// Receiver name/type are empty: a function_literal does not bind
			// a receiver in Go, so selector calls like x.Y() inside the body
			// are filtered out by collectCallNames' recvName check. Anything
			// resolvable (identifier or pkg.X) still lands.
			for _, ref := range collectCallNames(bodyNode, src, "", "") {
				if ref.pkg != "" {
					key := string(callerNode.ID) + callKeySep + ref.pkg + "." + ref.name
					if seenU[key] {
						continue
					}
					seenU[key] = true
					*unresolved = append(*unresolved, domain.UnresolvedCall{
						CallerID:     callerNode.ID,
						CalleeName:   ref.name,
						PkgQualifier: ref.pkg,
					})
					continue
				}
				calleeNode, ok := symbols[ref.name]
				if !ok {
					key := string(callerNode.ID) + callKeySep + ref.name
					if seenU[key] {
						continue
					}
					seenU[key] = true
					*unresolved = append(*unresolved, domain.UnresolvedCall{
						CallerID:   callerNode.ID,
						CalleeName: ref.name,
					})
					continue
				}
				key := string(callerNode.ID) + callKeySep + string(calleeNode.ID)
				if seen[key] {
					continue
				}
				seen[key] = true
				if e, err := domain.NewEdge(callerNode.ID, calleeNode.ID, domain.EdgeCalls,
					domain.WithConfidence(domain.Probable),
				); err == nil {
					*edges = append(*edges, e)
				}
			}
		}
	}
	// Always descend; function_literals can nest.
	count := int(node.ChildCount())
	for i := range count {
		collectAnonCalls(node.Child(i), src, symbols, callerNode, edges, unresolved, seen, seenU)
	}
}

// extractReceiverBinding returns the receiver's parameter name and type
// from a method_declaration receiver_node. For `func (s *Server) Foo()`,
// returns ("s", "Server"). Either may be empty (anonymous receiver, or
// no type identifier found); callers should skip when recvName is empty.
func extractReceiverBinding(receiverNode *sitter.Node, src []byte) (name, typ string) {
	typ = extractReceiverType(receiverNode, src)

	// Receiver is a parameter_list with a single parameter_declaration.
	// The parameter_declaration has 'name' (identifier) + 'type'. Walk
	// for the first identifier under a parameter_declaration.
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if name != "" {
			return
		}
		if n.Type() == "parameter_declaration" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name = string(src[nameNode.StartByte():nameNode.EndByte()])
				return
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(receiverNode)
	return name, typ
}

// collectCallNames does a depth-first walk of node and returns the lookup
// keys for every call_expression we can resolve against the file's symbol
// map. Three forms are recognised:
//
//   - identifier call:        foo()             → "foo"
//   - Go selector call:       recvName.X()      → "recvType.X"   (only when
//     recvName and recvType are non-empty and the operand is an identifier
//     equal to recvName — Go uses selector_expression)
//   - TS member call:         this.X() / r.X()  → "recvType.X"   (only when
//     recvName and recvType are non-empty and the object text equals
//     recvName — TS/TSX tree-sitter uses member_expression with a "this"
//     literal child rather than an identifier, so matching by text covers
//     both r.X() with recvName="r" and this.X() with recvName="this".
//     solov2-gv6.)
//
// Package-qualified selector calls (pkg.Bar()) are returned with callRef.pkg
// set so promotion can resolve them via the import map (see the cross-package
// note on extractCallEdges, solov2-xc51). Chained selectors (s.field.X()) whose
// operand is not a plain identifier are still skipped.
// callRef is one call site collected from a function/method body. name is the
// callee identifier (or "Receiver.method" for a resolved receiver call); pkg is
// the selector operand for a package-qualified call (the "cmd" in cmd.Execute()),
// empty for plain or receiver-local calls.
type callRef struct {
	name string
	pkg  string
}

func collectCallNames(node *sitter.Node, src []byte, recvName, recvType string) []callRef {
	var refs []callRef
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil {
				switch fn.Type() {
				case "identifier":
					refs = append(refs, callRef{name: string(src[fn.StartByte():fn.EndByte()])})
				case "selector_expression":
					operand := fn.ChildByFieldName("operand")
					field := fn.ChildByFieldName("field")
					if operand != nil && field != nil && operand.Type() == "identifier" {
						op := string(src[operand.StartByte():operand.EndByte()])
						fld := string(src[field.StartByte():field.EndByte()])
						if recvName != "" && recvType != "" && op == recvName {
							// s.foo() inside a method on *Server -> Server.foo (local).
							refs = append(refs, callRef{name: recvType + "." + fld})
						} else {
							// pkg.Foo() — package-qualified; resolved at
							// promotion via the import map (solov2-xc51). The
							// operand may also be a local variable; a
							// non-import qualifier simply misses there.
							refs = append(refs, callRef{name: fld, pkg: op})
						}
					}
				case "member_expression":
					if recvName != "" && recvType != "" {
						object := fn.ChildByFieldName("object")
						property := fn.ChildByFieldName("property")
						if object != nil && property != nil &&
							string(src[object.StartByte():object.EndByte()]) == recvName {
							refs = append(refs, callRef{name: recvType + "." + string(src[property.StartByte():property.EndByte()])})
						}
					}
				}
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(node)
	return refs
}

// extractImports walks the file's import declarations and returns a map from
// the local package identifier to its import path. For an explicit alias
// (import foo "x/y") the key is the alias; otherwise it is the path's last
// segment (import "x/y" -> "y"), which equals the package name in the common
// case. Blank ("_") and dot (".") imports are skipped — neither yields a
// usable qualifier (solov2-xc51).
func extractImports(root *sitter.Node, src []byte) map[string]string {
	imports := map[string]string{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "import_spec" {
			pathNode := n.ChildByFieldName("path")
			if pathNode != nil {
				path := strings.Trim(string(src[pathNode.StartByte():pathNode.EndByte()]), `"`)
				if path != "" {
					local := ""
					if nameNode := n.ChildByFieldName("name"); nameNode != nil {
						local = string(src[nameNode.StartByte():nameNode.EndByte()])
					}
					switch local {
					case "_", ".":
						// no usable qualifier
					case "":
						imports[lastPathSegment(path)] = path
					default:
						imports[local] = path
					}
				}
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(imports) == 0 {
		return nil
	}
	return imports
}

// lastPathSegment returns the final "/"-separated segment of an import path.
func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ----- misc helpers -----

func extractPackageName(root *sitter.Node, src []byte) string {
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child.Type() == "package_clause" {
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				// fallback: second child (package keyword is first)
				if child.ChildCount() >= 2 {
					nameNode = child.Child(1)
				}
			}
			if nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
		}
	}
	return ""
}

func extractReceiverType(receiverNode *sitter.Node, src []byte) string {
	// receiver is a parameter_list: ( receiverSpec )
	// walk looking for a type_identifier or pointer_type -> type_identifier
	var walk func(*sitter.Node) string
	walk = func(n *sitter.Node) string {
		if n.Type() == "type_identifier" {
			return string(src[n.StartByte():n.EndByte()])
		}
		count := int(n.ChildCount())
		for i := range count {
			if result := walk(n.Child(i)); result != "" {
				return result
			}
		}
		return ""
	}
	return walk(receiverNode)
}

func extractSignature(node *sitter.Node, src []byte) string {
	params := node.ChildByFieldName("parameters")
	result := node.ChildByFieldName("result")

	if params == nil {
		return ""
	}

	var sb strings.Builder
	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		sb.WriteString(string(src[nameNode.StartByte():nameNode.EndByte()]))
	}
	sb.WriteString(string(src[params.StartByte():params.EndByte()]))
	if result != nil {
		sb.WriteString(" ")
		sb.WriteString(string(src[result.StartByte():result.EndByte()]))
	}
	return sb.String()
}

func lineRange(node *sitter.Node) domain.LineRange {
	return domain.LineRange{
		Start: int(node.StartPoint().Row) + 1,
		End:   int(node.EndPoint().Row) + 1,
	}
}

func hasErrorNode(node *sitter.Node) bool {
	if node.IsError() || node.IsMissing() {
		return true
	}
	count := int(node.ChildCount())
	for i := range count {
		if hasErrorNode(node.Child(i)) {
			return true
		}
	}
	return false
}

// firstErrorFailure returns a ParseFailure describing the first ERROR or
// MISSING node found in a depth-first walk of the tree. If no such node is
// found (defensive — callers gate this with hasErrorNode), it returns a
// generic failure with Line 0.
func firstErrorFailure(node *sitter.Node) domain.ParseFailure {
	if node.IsError() {
		return domain.ParseFailure{
			Line:    int(node.StartPoint().Row) + 1,
			Message: "syntax error",
		}
	}
	if node.IsMissing() {
		return domain.ParseFailure{
			Line:    int(node.StartPoint().Row) + 1,
			Message: "missing token: " + node.Type(),
		}
	}
	count := int(node.ChildCount())
	for i := range count {
		child := node.Child(i)
		if hasErrorNode(child) {
			return firstErrorFailure(child)
		}
	}
	return domain.ParseFailure{Message: "syntax error"}
}

// nodeID produces a deterministic identifier for a node.
func nodeID(repoID, path string, kind domain.NodeKind, name string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", repoID, path, string(kind), name)
	return hex.EncodeToString(h.Sum(nil))
}
