// go_frameworks.go — framework-aware Go extraction (solov2-crn7, -qqqy).
//
// The generic symbol pass (go_query.go + symbols.scm) sees a framework
// command as an opaque `var x = &pkg.Type{...}` KindVariable named after
// the Go var. This pass recognises the framework struct-literals via
// frameworks.scm and promotes them to KindCommand nodes named by the
// framework's command word, then builds the command tree as CONTAINS
// edges so call_chain / blast_radius walk it .
//
// Frameworks:
//   - spf13/cobra — `var rootCmd = &cobra.Command{Use: ...}` named by the
//     first word of Use:; `parent.AddCommand(child)` → CONTAINS.
//   - urfave/cli  — `var app = &cli.App{Name: ..., Commands: []*cli.Command
//     {...}}` named by Name:; each Commands-slice literal → a CONTAINS
//     child (solov2-qqqy).
//   - gin/echo/chi — `router.METHOD("/path", handler)` → a KindRoute node
//     named "METHOD /path" plus a ROUTES route→handler reference, emitted
//     as a domain.UnresolvedCall{EdgeKind: EdgeRoutes} so the handler binds
//     through the same package-wide promotion resolver as a plain call
//     (solov2-ketg). The route is named with the method so GET and POST on
//     one path don't collide on the (repo,path,kind,name) promotion PK.
//   - alecthomas/kong — commands are struct FIELDS with `cmd:""` tags, not
//     composite literals; a `cmd:""`-tagged field → KindCommand named by the
//     dasherized field name (or a `name:` tag), nested via the field's struct
//     type (a command struct's own cmd fields are its subcommands). Runs as
//     its own struct-tag walk, independent of the @fwvar.* literal dispatch
//     (solov2-su6d).

package treesitter

import (
	"reflect"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// Module paths whose struct-literals we promote. Matched against the file
// import map so an unrelated `foo.Command{}` / `foo.App{}` literal can't
// masquerade as a framework command. urfaveImportPrefix is a prefix
// because urfave versions its module path (…/cli, …/cli/v2, …/cli/v3).
const (
	cobraImportPath    = "github.com/spf13/cobra"
	urfaveImportPrefix = "github.com/urfave/cli"
	kongImportPath     = "github.com/alecthomas/kong"
)

// ginEchoImportPrefixes / chiImportPrefix are the HTTP-router module paths
// whose router.METHOD(...) calls we promote to KindRoute nodes. echo and
// chi version their module path (…/echo/v4, …/chi/v5), so these match by
// prefix (solov2-ketg).
var ginEchoImportPrefixes = []string{
	"github.com/gin-gonic/gin",
	"github.com/labstack/echo",
}

const chiImportPrefix = "github.com/go-chi/chi"

// upperVerbs (gin/echo: GET) and titleVerbs (chi: Get) each map a router
// method's field name to the canonical upper-case HTTP method used in the
// route node name. They are kept per-framework, not flattened, because
// chi's title-case verbs collide with extremely common Go method names
// (cfg.Get, client.Post) — accepting them only when chi is the imported
// router keeps a gin/echo-only file from mistaking client.Post(...) for a
// route. This is the verb half of the precision gate; a field absent from
// the active set drops the selector-call match.
var upperVerbs = map[string]string{
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
	"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
	"CONNECT": "CONNECT", "TRACE": "TRACE",
}

var titleVerbs = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
	"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	"Connect": "CONNECT", "Trace": "TRACE",
}

// frameworkCommands is the framework pass result: the promoted
// KindCommand nodes, a binding from the Go var identifier to its command
// node (used to skip the generic var extraction, to resolve AddCommand
// arguments, and to keep anon-call attribution on the command rather than
// the package — solov2-zuvl), and the command-tree CONTAINS edges.
type frameworkCommands struct {
	nodes []*domain.Node
	byVar map[string]*domain.Node
	edges []*domain.Edge
	// unresolved carries route→handler references (KindRoute → handler)
	// the promoter binds against the package-wide symbol map, emitting a
	// ROUTES edge instead of CALLS (solov2-ketg). Routes don't appear in
	// byVar — no Go identifier references a route node.
	unresolved []domain.UnresolvedCall
}

// commandVar reports whether the Go var identifier name was promoted to a
// KindCommand node, so the generic var extractor can skip it. Safe on the
// zero value (nil map).
func (c frameworkCommands) commandVar(name string) bool {
	return c.byVar[name] != nil
}

// add records a command node under its Go var name (the byVar binding).
func (c *frameworkCommands) add(n *domain.Node, varName string) {
	c.nodes = append(c.nodes, n)
	c.byVar[varName] = n
}

// extractFrameworkCommands runs frameworks.scm over root and promotes
// recognised command struct-literals (cobra, urfave) to KindCommand nodes
// plus their command-tree CONTAINS edges. Returns the zero value when no
// supported framework is imported, so the caller no-ops the common file
// cheaply.
func extractFrameworkCommands(root *sitter.Node, src []byte, imports map[string]string, repoID, path string) frameworkCommands {
	_, cobraOK := cobraAlias(imports)
	urfaveOK := anyImportHasPrefix(imports, urfaveImportPrefix)
	ginEchoOK := anyPrefixImported(imports, ginEchoImportPrefixes)
	chiOK := anyImportHasPrefix(imports, chiImportPrefix)
	kongOK := anyImportHasPrefix(imports, kongImportPath)
	if !cobraOK && !urfaveOK && !ginEchoOK && !chiOK && !kongOK {
		return frameworkCommands{}
	}
	q, err := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "frameworks")
	if err != nil || q == nil {
		return frameworkCommands{}
	}
	matches := runQuery(q, root)

	p := fwParse{src: src, imports: imports, repoID: repoID, path: path, cobraOK: cobraOK, urfaveOK: urfaveOK, ginEchoOK: ginEchoOK, chiOK: chiOK, kongOK: kongOK}
	fw := frameworkCommands{byVar: map[string]*domain.Node{}}
	for _, m := range matches {
		fw.dispatchVar(m, p)
	}
	// Second pass: resolve the by-reference command-tree wire-ups now that
	// every command var is in byVar (a child may be declared after its
	// parent). cobra: parent.AddCommand(child); urfave: App.Commands slice
	// identifier elements. Only when a command var was actually promoted.
	if len(fw.byVar) > 0 {
		fw.edges = append(fw.edges, cobraContainsEdges(matches, src, fw.byVar)...)
		fw.edges = append(fw.edges, urfaveRefContainsEdges(matches, p, fw.byVar)...)
	}
	// Routes are independent of the command tree: each router.METHOD(path,
	// handler) call becomes a KindRoute node + a ROUTES route→handler
	// UnresolvedCall, resolved at promotion (solov2-ketg).
	if ginEchoOK || chiOK {
		fw.addRoutes(matches, p)
	}
	// kong models commands as struct fields with `cmd:""` tags, not
	// composite literals, so it runs independently of the @fwvar.* dispatch
	// (solov2-su6d).
	if kongOK {
		fw.addKongCommands(matches, p)
	}
	if len(fw.nodes) == 0 && len(fw.edges) == 0 && len(fw.unresolved) == 0 {
		return frameworkCommands{}
	}
	return fw
}

// fwParse bundles the per-file inputs the framework builders share, so
// the dispatch + builders stay within the argument-count budget.
type fwParse struct {
	src          []byte
	imports      map[string]string
	repoID, path string
	cobraOK      bool
	urfaveOK     bool
	ginEchoOK    bool
	chiOK        bool
	kongOK       bool
}

// dispatchVar classifies one @fwvar.* match by (resolved import, type
// name) and routes it to the matching framework builder. Non-framework
// vars and unsupported types fall through silently (they stay
// KindVariable via the generic pass).
func (c *frameworkCommands) dispatchVar(m queryMatch, p fwParse) {
	pkgNode := m.node("fwvar.pkg")
	typeNode := m.node("fwvar.type")
	if pkgNode == nil || typeNode == nil {
		return
	}
	pkg := string(p.src[pkgNode.StartByte():pkgNode.EndByte()])
	typ := string(p.src[typeNode.StartByte():typeNode.EndByte()])
	switch {
	case p.cobraOK && typ == "Command" && p.imports[pkg] == cobraImportPath:
		// cobra names a command by the first word of Use: ("verb [args]").
		if n, varName := buildNamedCommandVar(m, p.src, p.repoID, p.path, "Use"); n != nil {
			c.add(n, varName)
		}
	case p.urfaveOK && typ == "Command" && isUrfavePkg(pkg, p.imports):
		// urfave by-reference subcommand: `var addCmd = &cli.Command{Name:
		// ...}`, linked into an App's Commands slice by identifier (resolved
		// in urfaveRefContainsEdges). Named by Name:.
		if n, varName := buildNamedCommandVar(m, p.src, p.repoID, p.path, "Name"); n != nil {
			c.add(n, varName)
		}
	case p.urfaveOK && typ == "App" && isUrfavePkg(pkg, p.imports):
		c.addUrfaveApp(m, p.src, p.repoID, p.path)
	}
}

// cobraAlias returns the local identifier the file uses for spf13/cobra
// (the import path's last segment, or an explicit alias), and whether the
// package is imported at all.
func cobraAlias(imports map[string]string) (string, bool) {
	for local, path := range imports {
		if path == cobraImportPath {
			return local, true
		}
	}
	return "", false
}

// isUrfavePkg reports whether the source qualifier pkg refers to
// urfave/cli. urfave's package name is always "cli" regardless of the
// versioned module path, but tree-sitter's import map keys a non-aliased
// `…/cli/v2` import under "v2" (the path's last segment), so an exact
// alias lookup misses; the bare "cli" fallback covers that case (the
// caller already verified urfave is imported in the file).
func isUrfavePkg(pkg string, imports map[string]string) bool {
	if strings.HasPrefix(imports[pkg], urfaveImportPrefix) {
		return true
	}
	return pkg == "cli"
}

// anyImportHasPrefix reports whether any imported path starts with prefix.
func anyImportHasPrefix(imports map[string]string, prefix string) bool {
	for _, path := range imports {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// buildNamedCommandVar promotes one command-var match to a KindCommand
// node and returns it with the Go var identifier (the byVar binding key).
// The display Name is the first word of the struct field nameKey (cobra's
// Use: "verb [args]" → "verb"; urfave's Name: → itself), so
// eng_find_symbol hits the command word rather than the Go var name. A
// missing/empty nameKey field returns nil — there's nothing meaningful to
// name the command, so it stays a generic KindVariable.
func buildNamedCommandVar(m queryMatch, src []byte, repoID, path, nameKey string) (*domain.Node, string) {
	varNode := m.node("fwvar.name")
	body := m.node("fwvar.body")
	decl := m.node("fwvar.decl")
	if varNode == nil || body == nil || decl == nil {
		return nil, ""
	}
	varName := string(src[varNode.StartByte():varNode.EndByte()])
	cmdName := firstWord(keyedStringValue(body, src, nameKey))
	if cmdName == "" {
		return nil, ""
	}
	// Node identity keys on (repo, path, kind, name); two sibling commands
	// could in principle share a Use word, so the ID is disambiguated by
	// the Go var name (unique at package scope) while the human-facing Name
	// stays the command word — same tactic as the init@L disambiguator in
	// buildFunctionNodeFromCaptures.
	return newCommandNode(nodeID(repoID, path, domain.KindCommand, varName), cmdName, decl, src, path), varName
}

// addUrfaveApp promotes a `var app = &cli.App{Name: ..., Commands: []*cli.
// Command{...}}` match: the app itself becomes a KindCommand named by
// Name:, and every literal in its Commands slice becomes a child
// KindCommand (named by its own Name:) with an app→child CONTAINS edge.
// Subcommands are anonymous literals, so their node IDs are disambiguated
// by "appVar/subName" and they are NOT added to byVar (no Go identifier
// references them). Nested Subcommands are deferred to a follow-up.
func (c *frameworkCommands) addUrfaveApp(m queryMatch, src []byte, repoID, path string) {
	varNode := m.node("fwvar.name")
	body := m.node("fwvar.body")
	decl := m.node("fwvar.decl")
	if varNode == nil || body == nil || decl == nil {
		return
	}
	varName := string(src[varNode.StartByte():varNode.EndByte()])
	appName := firstWord(keyedStringValue(body, src, "Name"))
	if appName == "" {
		return
	}
	app := newCommandNode(nodeID(repoID, path, domain.KindCommand, varName), appName, decl, src, path)
	if app == nil {
		return
	}
	c.add(app, varName)
	set := newContainsEdgeSet()
	for _, sub := range urfaveSubcommandLiterals(body, src) {
		subName := firstWord(keyedStringValue(sub, src, "Name"))
		if subName == "" {
			continue
		}
		child := newCommandNode(nodeID(repoID, path, domain.KindCommand, varName+"/"+subName), subName, sub, src, path)
		if child == nil {
			continue
		}
		c.nodes = append(c.nodes, child)
		set.add(app.ID, child.ID)
	}
	c.edges = append(c.edges, set.edges...)
}

// urfaveCommandsBody returns the literal_value listing an App's
// subcommands — the body of its `Commands: []*cli.Command{ ... }` field —
// or nil when there is no Commands slice.
func urfaveCommandsBody(body *sitter.Node, src []byte) *sitter.Node {
	commands := keyedElementValue(body, src, "Commands")
	if commands == nil || commands.Type() != "composite_literal" {
		return nil
	}
	return commands.ChildByFieldName("body")
}

// urfaveSubcommandLiterals returns the inline `{Name: ...}` literal_value
// nodes in an App's Commands slice (the anonymous-literal idiom).
func urfaveSubcommandLiterals(body *sitter.Node, src []byte) []*sitter.Node {
	inner := urfaveCommandsBody(body, src)
	if inner == nil {
		return nil
	}
	var out []*sitter.Node
	named := int(inner.NamedChildCount())
	for i := range named {
		el := inner.NamedChild(i)
		if el == nil || el.Type() != "literal_element" || int(el.NamedChildCount()) == 0 {
			continue
		}
		if v := el.NamedChild(0); v != nil && v.Type() == "literal_value" {
			out = append(out, v)
		}
	}
	return out
}

// urfaveSubcommandRefs returns the identifier names in an App's Commands
// slice (the by-reference idiom `Commands: []*cli.Command{addCmd}`),
// resolved to command nodes by urfaveRefContainsEdges.
func urfaveSubcommandRefs(body *sitter.Node, src []byte) []string {
	inner := urfaveCommandsBody(body, src)
	if inner == nil {
		return nil
	}
	var out []string
	named := int(inner.NamedChildCount())
	for i := range named {
		el := inner.NamedChild(i)
		if el == nil || el.Type() != "literal_element" || int(el.NamedChildCount()) == 0 {
			continue
		}
		if v := el.NamedChild(0); v != nil && v.Type() == "identifier" {
			out = append(out, string(src[v.StartByte():v.EndByte()]))
		}
	}
	return out
}

// urfaveRefContainsEdges resolves the by-reference subcommands of every
// urfave App (`Commands: []*cli.Command{addCmd}`) to app→child CONTAINS
// edges via byVar. Run as a second pass so a subcommand var declared
// after its App still resolves. Dedup is per (app, child).
func urfaveRefContainsEdges(matches []queryMatch, p fwParse, byVar map[string]*domain.Node) []*domain.Edge {
	set := newContainsEdgeSet()
	for _, m := range matches {
		typeNode := m.node("fwvar.type")
		pkgNode := m.node("fwvar.pkg")
		varNode := m.node("fwvar.name")
		body := m.node("fwvar.body")
		if typeNode == nil || pkgNode == nil || varNode == nil || body == nil {
			continue
		}
		if string(p.src[typeNode.StartByte():typeNode.EndByte()]) != "App" ||
			!isUrfavePkg(string(p.src[pkgNode.StartByte():pkgNode.EndByte()]), p.imports) {
			continue
		}
		app := byVar[string(p.src[varNode.StartByte():varNode.EndByte()])]
		if app == nil {
			continue
		}
		for _, name := range urfaveSubcommandRefs(body, p.src) {
			if child := byVar[name]; child != nil {
				set.add(app.ID, child.ID)
			}
		}
	}
	return set.edges
}

// anyPrefixImported reports whether any imported path starts with one of
// prefixes — the file half of the route precision gate.
func anyPrefixImported(imports map[string]string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if anyImportHasPrefix(imports, prefix) {
			return true
		}
	}
	return false
}

// routeMethod resolves a router method's field name to its canonical
// upper-case HTTP method, accepting upper-case verbs only when gin/echo is
// the imported router and title-case verbs only when chi is — so a
// gin-only file's client.Post(...) or a chi-only file's resp.GET(...) is
// not mistaken for a route. ok=false when the field is not an active verb.
func routeMethod(field string, ginEchoOK, chiOK bool) (string, bool) {
	if ginEchoOK {
		if m, ok := upperVerbs[field]; ok {
			return m, true
		}
	}
	if chiOK {
		if m, ok := titleVerbs[field]; ok {
			return m, true
		}
	}
	return "", false
}

// addRoutes promotes every router.METHOD("/path", handler) selector call to
// a KindRoute node named "METHOD /path" and records the route→handler
// reference as a ROUTES UnresolvedCall (bound at promotion). The precision
// gate — framework-specific verb field, string-literal path, present
// handler arg, on top of the file-level router import already checked by
// the caller — reduces but cannot eliminate misfires: the router is a
// param of an unresolved type, so a same-file someVar.GET("x", y) in a
// gin/echo file still can't be told apart from a real route (an accepted
// v1 limitation). Group/Route/Mount and middleware nesting are deferred;
// only flat r.METHOD(path, handler) is matched. Dedup is per route name
// within the file (solov2-ketg).
func (c *frameworkCommands) addRoutes(matches []queryMatch, p fwParse) {
	seen := map[string]bool{}
	for _, m := range matches {
		methodNode := m.node("route.method")
		argsNode := m.node("route.args")
		callNode := m.node("route.call")
		if methodNode == nil || argsNode == nil || callNode == nil {
			continue
		}
		method, ok := routeMethod(string(p.src[methodNode.StartByte():methodNode.EndByte()]), p.ginEchoOK, p.chiOK)
		if !ok {
			continue
		}
		path, handler := routeArgs(argsNode, p.src)
		if path == "" || handler == nil {
			continue
		}
		name := method + " " + path
		if seen[name] {
			continue
		}
		seen[name] = true
		route := newRouteNode(nodeID(p.repoID, p.path, domain.KindRoute, name), name, callNode, p.src, p.path)
		if route == nil {
			continue
		}
		c.nodes = append(c.nodes, route)
		if uc, ok := routeHandlerUnresolved(route.ID, handler, callNode, p.src); ok {
			c.unresolved = append(c.unresolved, uc)
		}
	}
}

// routeArgs extracts the route's path (first arg, which must be a string
// literal) and handler (the second arg) from a router.METHOD argument
// list. Returns ("", nil) when the first arg is not a string literal or
// there is no handler arg — the literal-path + handler-present halves of
// the precision gate. The handler is taken as the second positional arg,
// which is correct for echo/chi and the common single-handler gin form
// r.GET(path, handler); a gin route with leading middleware
// (r.GET(path, mw, handler)) attributes to the middleware in v1.
func routeArgs(argsNode *sitter.Node, src []byte) (string, *sitter.Node) {
	if int(argsNode.NamedChildCount()) < 2 {
		return "", nil
	}
	first := argsNode.NamedChild(0)
	if first == nil {
		return "", nil
	}
	switch first.Type() {
	case "interpreted_string_literal", "raw_string_literal":
	default:
		return "", nil
	}
	path := strings.Trim(string(src[first.StartByte():first.EndByte()]), "\"`")
	if path == "" {
		return "", nil
	}
	return path, argsNode.NamedChild(1)
}

// routeHandlerUnresolved builds the ROUTES route→handler UnresolvedCall for
// a handler argument. A bare identifier (h) resolves by name against the
// route file's package; a package/receiver selector (pkg.Handler) resolves
// via the import map (a local-var receiver simply won't match, so no false
// edge is emitted). Func-literal and other handler forms produce no edge
// (ok=false), mirroring the deferred urfave Action-closure case. The call
// site's line is carried so the resolved edge attributes to the route
// registration (solov2-ketg).
func routeHandlerUnresolved(routeID domain.NodeID, handler, callNode *sitter.Node, src []byte) (domain.UnresolvedCall, bool) {
	uc := domain.UnresolvedCall{CallerID: routeID, EdgeKind: domain.EdgeRoutes, SrcLine: lineRange(callNode).Start}
	switch handler.Type() {
	case "identifier":
		uc.CalleeName = string(src[handler.StartByte():handler.EndByte()])
		return uc, true
	case "selector_expression":
		operand := handler.ChildByFieldName("operand")
		field := handler.ChildByFieldName("field")
		if operand == nil || field == nil || operand.Type() != "identifier" {
			return domain.UnresolvedCall{}, false
		}
		uc.PkgQualifier = string(src[operand.StartByte():operand.EndByte()])
		uc.CalleeName = string(src[field.StartByte():field.EndByte()])
		return uc, true
	default:
		return domain.UnresolvedCall{}, false
	}
}

// newRouteNode builds a KindRoute node named "METHOD /path", with lines +
// raw content from the router.METHOD(...) call expression.
func newRouteNode(id, name string, callNode *sitter.Node, src []byte, path string) *domain.Node {
	n, err := domain.NewNode(
		domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindRoute},
		domain.WithLanguage("go"),
		domain.WithLines(lineRange(callNode)),
		domain.WithRawContent(string(src[callNode.StartByte():callNode.EndByte()])),
	)
	if err != nil {
		return nil
	}
	return n
}

// newCommandNode builds a KindCommand node whose lines + raw content come
// from srcNode (the declaration or subcommand literal). Returns nil if the
// domain constructor rejects the spec.
func newCommandNode(id string, name string, srcNode *sitter.Node, src []byte, path string) *domain.Node {
	n, err := domain.NewNode(
		domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindCommand},
		domain.WithLanguage("go"),
		domain.WithLines(lineRange(srcNode)),
		domain.WithRawContent(string(src[srcNode.StartByte():srcNode.EndByte()])),
		domain.WithExported(goExported(name)),
	)
	if err != nil {
		return nil
	}
	return n
}

// containsEdge builds a Definite CONTAINS edge src→tgt, or nil on error.
func containsEdge(srcID, tgtID domain.NodeID) *domain.Edge {
	e, err := domain.NewEdge(
		domain.EdgeSpec{Src: srcID, Tgt: tgtID, Kind: domain.EdgeContains},
		domain.WithConfidence(domain.Definite),
	)
	if err != nil {
		return nil
	}
	return e
}

// containsEdgeSet accumulates command-tree CONTAINS edges, deduped per
// (parent, child) — the wire-up shared by every command framework (cobra
// AddCommand, urfave Commands slice, kong nested struct types). Each
// recogniser differs only in how it resolves the parent/child pair; this
// collapses the identical "key, skip-if-seen, build edge" tail. A repeated
// registration of the same pair yields one edge; an edge the constructor
// rejects is silently dropped.
type containsEdgeSet struct {
	edges []*domain.Edge
	seen  map[string]bool
}

func newContainsEdgeSet() containsEdgeSet {
	return containsEdgeSet{seen: map[string]bool{}}
}

func (s *containsEdgeSet) add(parent, child domain.NodeID) {
	key := string(parent) + callKeySep + string(child)
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	if e := containsEdge(parent, child); e != nil {
		s.edges = append(s.edges, e)
	}
}

// cobraContainsEdges turns every parent.AddCommand(child, ...) call whose
// parent and child(ren) both resolve to command nodes into parent→child
// CONTAINS edges. Dedup is per (parent, child) so repeated registrations
// produce one edge. Confidence is Definite — it's a literal wire-up, not
// an inferred relationship.
func cobraContainsEdges(matches []queryMatch, src []byte, byVar map[string]*domain.Node) []*domain.Edge {
	set := newContainsEdgeSet()
	for _, m := range matches {
		methodNode := m.node("cobra.add.method")
		parentNode := m.node("cobra.add.parent")
		argsNode := m.node("cobra.add.args")
		if methodNode == nil || parentNode == nil || argsNode == nil {
			continue
		}
		if string(src[methodNode.StartByte():methodNode.EndByte()]) != "AddCommand" {
			continue
		}
		parent := byVar[string(src[parentNode.StartByte():parentNode.EndByte()])]
		if parent == nil {
			continue
		}
		named := int(argsNode.NamedChildCount())
		for i := range named {
			arg := argsNode.NamedChild(i)
			if arg == nil || arg.Type() != "identifier" {
				continue
			}
			if child := byVar[string(src[arg.StartByte():arg.EndByte()])]; child != nil {
				set.add(parent.ID, child.ID)
			}
		}
	}
	return set.edges
}

// kongField is one `cmd:""`-tagged struct field promoted to a command: the
// command node plus the bare name of the field's struct type, used to nest
// subcommands (a command whose struct type has its own cmd fields CONTAINS
// them).
type kongField struct {
	node     *domain.Node
	declType string // bare type identifier of the field, "" if not a plain type
}

// addKongCommands promotes alecthomas/kong struct-tag commands. Each
// `cmd:""`-tagged field of a struct becomes a KindCommand node named by its
// `name:` tag or the dasherized field name. Nesting follows the field type,
// not Go embedding: a command field of type T is the parent of every cmd
// field declared on struct T, so the root CLI struct's fields fall out as
// top-level commands (nothing has the CLI struct as a field type). Two
// passes over the matches: build every command + the type→command index
// first (a child struct may be declared before or after its parent), then
// wire CONTAINS (solov2-su6d). v1 covers named fields with a plain (or
// pointer) struct type; embedded/anonymous-struct command fields are
// deferred.
func (c *frameworkCommands) addKongCommands(matches []queryMatch, p fwParse) {
	// byType: bare struct type → the command node whose field has that type,
	// i.e. the parent of that struct's own cmd fields.
	byType := map[string]*domain.Node{}
	// fieldsOf: struct type name → its promoted cmd-field commands.
	fieldsOf := map[string][]kongField{}
	for _, m := range matches {
		structName, kf, ok := c.buildKongField(m, p)
		if !ok {
			continue
		}
		c.nodes = append(c.nodes, kf.node)
		fieldsOf[structName] = append(fieldsOf[structName], kf)
		if kf.declType != "" {
			byType[kf.declType] = kf.node
		}
	}
	set := newContainsEdgeSet()
	for structName, fields := range fieldsOf {
		parent := byType[structName]
		if parent == nil {
			continue // root struct: its fields stay top-level commands
		}
		for _, kf := range fields {
			set.add(parent.ID, kf.node.ID)
		}
	}
	c.edges = append(c.edges, set.edges...)
}

// buildKongField promotes one @kong.* match to a command node when its tag
// carries a `cmd` key. Returns the containing struct's type name (the
// fieldsOf / byType key), the field, and ok=false for non-command fields
// (arg:/flag tags also match the query but lack a cmd key).
func (c *frameworkCommands) buildKongField(m queryMatch, p fwParse) (string, kongField, bool) {
	structNode := m.node("kong.struct.name")
	nameNode := m.node("kong.field.name")
	typeNode := m.node("kong.field.type")
	tagNode := m.node("kong.field.tag")
	declNode := m.node("kong.field.decl")
	if structNode == nil || nameNode == nil || typeNode == nil || tagNode == nil || declNode == nil {
		return "", kongField{}, false
	}
	tag := reflect.StructTag(strings.Trim(string(p.src[tagNode.StartByte():tagNode.EndByte()]), "`"))
	if _, isCmd := tag.Lookup("cmd"); !isCmd {
		return "", kongField{}, false
	}
	structName := string(p.src[structNode.StartByte():structNode.EndByte()])
	fieldName := string(p.src[nameNode.StartByte():nameNode.EndByte()])
	cmdName := kongCommandName(fieldName, tag)
	if cmdName == "" {
		return "", kongField{}, false
	}
	// ID disambiguated by struct+field (the command word can repeat across
	// sibling structs), human name stays the command word — same tactic as
	// the cobra/urfave var disambiguator.
	id := nodeID(p.repoID, p.path, domain.KindCommand, structName+"/"+fieldName)
	// Source node is the whole field declaration (lines + raw content),
	// matching the cobra/urfave commands which store the full declaration.
	n := newCommandNode(id, cmdName, declNode, p.src, p.path)
	if n == nil {
		return "", kongField{}, false
	}
	return structName, kongField{node: n, declType: kongBareType(typeNode, p.src)}, true
}

// kongCommandName derives a kong command's display name: the `name:` tag
// when set, otherwise the dasherized field name (kong lowercases and inserts
// a dash at camelCase boundaries — ListUsers → list-users).
func kongCommandName(fieldName string, tag reflect.StructTag) string {
	if name := tag.Get("name"); name != "" {
		return name
	}
	return dasherize(fieldName)
}

// dasherize lowercases s and inserts a dash before each interior upper-case
// run boundary, approximating kong's field-name → command-name derivation.
func dasherize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// kongBareType returns the bare type-identifier name of a field type,
// unwrapping a single pointer (`*ServerCmd` → "ServerCmd"). Returns "" for
// qualified, slice, map, or anonymous-struct types — those don't name a
// local command struct, so the field has no nestable children.
func kongBareType(typeNode *sitter.Node, src []byte) string {
	switch typeNode.Type() {
	case "type_identifier":
		return string(src[typeNode.StartByte():typeNode.EndByte()])
	case "pointer_type":
		if inner := typeNode.NamedChild(0); inner != nil && inner.Type() == "type_identifier" {
			return string(src[inner.StartByte():inner.EndByte()])
		}
	}
	return ""
}

// keyedStringValue returns the string value of the keyed_element named key
// in a composite-literal body (`Use: "tool"` → "tool"), or "" when absent
// or non-string. Handles both interpreted ("…") and raw (`…`) literals.
func keyedStringValue(body *sitter.Node, src []byte, key string) string {
	v := keyedElementValue(body, src, key)
	if v == nil {
		return ""
	}
	switch v.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		return strings.Trim(string(src[v.StartByte():v.EndByte()]), "\"`")
	}
	return ""
}

// keyedElementValue returns the inner value node of the keyed_element
// named key in a composite-literal body, or nil when absent. The
// literal_value's keyed_element children wrap a literal_element(key
// identifier) and a literal_element(value); this returns the value
// element's inner node (a string literal, composite_literal, etc.).
func keyedElementValue(body *sitter.Node, src []byte, key string) *sitter.Node {
	named := int(body.NamedChildCount())
	for i := range named {
		ke := body.NamedChild(i)
		if ke == nil || ke.Type() != "keyed_element" || int(ke.NamedChildCount()) < 2 {
			continue
		}
		if literalElementText(ke.NamedChild(0), src, "identifier") != key {
			continue
		}
		vEl := ke.NamedChild(1)
		if vEl == nil || int(vEl.NamedChildCount()) == 0 {
			return nil
		}
		return vEl.NamedChild(0)
	}
	return nil
}

// literalElementText returns the text of a literal_element's inner node
// when that node has the wanted type, else "". Struct keys/values are
// wrapped one level deep (literal_element → identifier / string).
func literalElementText(el *sitter.Node, src []byte, wantType string) string {
	if el == nil || int(el.NamedChildCount()) == 0 {
		return ""
	}
	inner := el.NamedChild(0)
	if inner == nil || inner.Type() != wantType {
		return ""
	}
	return string(src[inner.StartByte():inner.EndByte()])
}

// firstWord returns the first whitespace-delimited token of s, or "".
// cobra's Use: is a "verb [positional args]" usage string, so the verb is
// the command name; urfave's Name: is already a single token.
func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
