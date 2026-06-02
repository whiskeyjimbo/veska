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
//
// Reserved follow-ups (separate beads): alecthomas/kong (struct tags) and
// HTTP routers (gin/echo/chi → KindRoute / EdgeRoutes — the cross-file
// handler edge needs promotion-resolver support beyond today's
// CALLS-only path). Each lands as another branch here.
package treesitter

import (
	"strings"

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
)

// frameworkCommands is the framework pass result: the promoted
// KindCommand nodes, a binding from the Go var identifier to its command
// node (used to skip the generic var extraction, to resolve AddCommand
// arguments, and to keep anon-call attribution on the command rather than
// the package — solov2-zuvl), and the command-tree CONTAINS edges.
type frameworkCommands struct {
	nodes []*domain.Node
	byVar map[string]*domain.Node
	edges []*domain.Edge
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
	if !cobraOK && !urfaveOK {
		return frameworkCommands{}
	}
	q, err := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "frameworks")
	if err != nil || q == nil {
		return frameworkCommands{}
	}
	matches := runQuery(q, root)

	p := fwParse{src: src, imports: imports, repoID: repoID, path: path, cobraOK: cobraOK, urfaveOK: urfaveOK}
	fw := frameworkCommands{byVar: map[string]*domain.Node{}}
	for _, m := range matches {
		fw.dispatchVar(m, p)
	}
	if len(fw.byVar) == 0 {
		return frameworkCommands{}
	}
	// Second pass: resolve the by-reference command-tree wire-ups now that
	// every command var is in byVar (a child may be declared after its
	// parent). cobra: parent.AddCommand(child); urfave: App.Commands slice
	// identifier elements.
	fw.edges = append(fw.edges, cobraContainsEdges(matches, src, fw.byVar)...)
	fw.edges = append(fw.edges, urfaveRefContainsEdges(matches, p, fw.byVar)...)
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
		if e := containsEdge(app.ID, child.ID); e != nil {
			c.edges = append(c.edges, e)
		}
	}
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
	var edges []*domain.Edge
	seen := map[string]bool{}
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
			child := byVar[name]
			if child == nil {
				continue
			}
			key := string(app.ID) + callKeySep + string(child.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			if e := containsEdge(app.ID, child.ID); e != nil {
				edges = append(edges, e)
			}
		}
	}
	return edges
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

// cobraContainsEdges turns every parent.AddCommand(child, ...) call whose
// parent and child(ren) both resolve to command nodes into parent→child
// CONTAINS edges. Dedup is per (parent, child) so repeated registrations
// produce one edge. Confidence is Definite — it's a literal wire-up, not
// an inferred relationship.
func cobraContainsEdges(matches []queryMatch, src []byte, byVar map[string]*domain.Node) []*domain.Edge {
	var edges []*domain.Edge
	seen := map[string]bool{}
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
			child := byVar[string(src[arg.StartByte():arg.EndByte()])]
			if child == nil {
				continue
			}
			key := string(parent.ID) + callKeySep + string(child.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			if e := containsEdge(parent.ID, child.ID); e != nil {
				edges = append(edges, e)
			}
		}
	}
	return edges
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
