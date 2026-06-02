// go_frameworks.go — framework-aware Go extraction (solov2-crn7).
//
// The generic symbol pass (go_query.go + symbols.scm) sees a cobra
// command as an opaque `var rootCmd = &cobra.Command{...}` KindVariable
// named "rootCmd". This pass recognises the cobra struct-literal via
// frameworks.scm and promotes it to a KindCommand node named by the
// first word of its Use: field, then turns `parent.AddCommand(child)`
// wire-up calls into parent→child CONTAINS edges so call_chain /
// blast_radius walk the actual command tree .
//
// First (and currently only) framework: spf13/cobra. urfave/cli, kong,
// and HTTP routers (gin/echo → KindRoute / EdgeRoutes) are reserved
// follow-ups; each is a new pattern in frameworks.scm plus a branch
// here, no change to the generic symbol/call extractors.
package treesitter

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// cobraImportPath is the module path whose Command struct-literal we
// promote. Matched against the file import map so an unrelated
// `foo.Command{}` literal can't masquerade as a cobra command.
const cobraImportPath = "github.com/spf13/cobra"

// cobraCommands is the framework pass result: the promoted KindCommand
// nodes, a binding from the Go var identifier to its command node (used
// both to skip the generic var extraction and to resolve AddCommand
// arguments), and the CONTAINS edges built from AddCommand(...) calls.
type cobraCommands struct {
	nodes []*domain.Node
	byVar map[string]*domain.Node
	edges []*domain.Edge
}

// commandVar reports whether the Go var identifier name was promoted to
// a KindCommand node, so the generic var extractor can skip it. Safe on
// the zero value (nil map).
func (c cobraCommands) commandVar(name string) bool {
	return c.byVar[name] != nil
}

// extractCobraCommands runs frameworks.scm over root and, when the file
// imports spf13/cobra, promotes each `var X = &cobra.Command{Use: ...}`
// to a KindCommand node named by the first word of its Use: field, then
// turns parent.AddCommand(child, ...) calls into parent→child CONTAINS
// edges. Returns the zero value (no nodes, no edges) when cobra isn't
// imported or no command vars are present, so the caller no-ops the
// common non-cobra file cheaply.
func extractCobraCommands(root *sitter.Node, src []byte, imports map[string]string, repoID, path string) cobraCommands {
	alias, ok := cobraAlias(imports)
	if !ok {
		return cobraCommands{}
	}
	q, err := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "frameworks")
	if err != nil || q == nil {
		return cobraCommands{}
	}
	matches := runQuery(q, root)

	byVar := map[string]*domain.Node{}
	var nodes []*domain.Node
	for _, m := range matches {
		pkgNode := m.node("cobra.cmd.pkg")
		if pkgNode == nil || string(src[pkgNode.StartByte():pkgNode.EndByte()]) != alias {
			continue
		}
		n, varName := buildCobraCommandNode(m, src, repoID, path)
		if n == nil {
			continue
		}
		nodes = append(nodes, n)
		byVar[varName] = n
	}
	if len(byVar) == 0 {
		return cobraCommands{}
	}
	return cobraCommands{
		nodes: nodes,
		byVar: byVar,
		edges: cobraContainsEdges(matches, src, byVar),
	}
}

// cobraAlias returns the local identifier the file uses for spf13/cobra
// (the import path's last segment, or an explicit alias), and whether
// the package is imported at all.
func cobraAlias(imports map[string]string) (string, bool) {
	for local, path := range imports {
		if path == cobraImportPath {
			return local, true
		}
	}
	return "", false
}

// buildCobraCommandNode promotes one command-var match to a KindCommand
// node and returns it with the Go var identifier (the byVar binding key).
// The display Name is the first word of the Use: field (cobra's
// "verb [args]" convention), so eng_find_symbol "version" hits the
// command rather than its Go var name. A missing/empty Use: returns nil
// — there's nothing meaningful to name the command, so the caller leaves
// it as a generic KindVariable.
func buildCobraCommandNode(m queryMatch, src []byte, repoID, path string) (*domain.Node, string) {
	varNode := m.node("cobra.cmd.var")
	body := m.node("cobra.cmd.body")
	decl := m.node("cobra.cmd.decl")
	if varNode == nil || body == nil || decl == nil {
		return nil, ""
	}
	varName := string(src[varNode.StartByte():varNode.EndByte()])
	cmdName := firstWord(keyedStringValue(body, src, "Use"))
	if cmdName == "" {
		return nil, ""
	}
	lr := lineRange(decl)
	raw := string(src[decl.StartByte():decl.EndByte()])
	// Node identity keys on (repo, path, kind, name); two sibling commands
	// could in principle share a Use word, so the ID is disambiguated by
	// the Go var name (unique at package scope) while the human-facing
	// Name stays the command word — same tactic as the init@L disambiguator
	// in buildFunctionNodeFromCaptures.
	id := nodeID(repoID, path, domain.KindCommand, varName)
	n, err := domain.NewNode(
		domain.NodeSpec{ID: id, Path: path, Name: cmdName, Kind: domain.KindCommand},
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(varName)),
	)
	if err != nil {
		return nil, ""
	}
	return n, varName
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
			e, err := domain.NewEdge(
				domain.EdgeSpec{Src: parent.ID, Tgt: child.ID, Kind: domain.EdgeContains},
				domain.WithConfidence(domain.Definite),
			)
			if err == nil {
				edges = append(edges, e)
			}
		}
	}
	return edges
}

// keyedStringValue returns the string value of the keyed_element named
// key in a composite-literal body (`Use: "tool"` → "tool"), or "" when
// absent or non-string. The literal_value's keyed_element children wrap
// a literal_element(key-identifier) and a literal_element(value).
func keyedStringValue(body *sitter.Node, src []byte, key string) string {
	named := int(body.NamedChildCount())
	for i := range named {
		ke := body.NamedChild(i)
		if ke == nil || ke.Type() != "keyed_element" || int(ke.NamedChildCount()) < 2 {
			continue
		}
		if literalElementText(ke.NamedChild(0), src, "identifier") != key {
			continue
		}
		v := literalElementText(ke.NamedChild(1), src, "interpreted_string_literal")
		return strings.Trim(v, "\"`")
	}
	return ""
}

// literalElementText returns the text of a literal_element's inner node
// when that node has the wanted type, else "". cobra struct keys/values
// are wrapped one level deep (literal_element → identifier / string).
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
// cobra's Use: is a "verb [positional args]" usage string, so the verb
// is the command name.
func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
