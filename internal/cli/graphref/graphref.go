// Package graphref holds the delivery-layer helper that resolves an opaque
// cross-repo node_id to a "symbol in file:line" projection and renders one
// side of a cross-repo edge. It is shared by the `veska symbol`/`veska
// context` (symbolcmd) and `veska calls`/`veska blast` (graphcmd) surfaces,
// both of which decorate cross-repo edges with the calling/called symbol
// instead of a bare hash (). Extracted from
// cmd/veska as part of the cmd = Cobra glue / logic-in-packages split
package graphref

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
)

// NodeInfo is the projection ResolveCrossRepoNode returns for cross-repo
// edge rendering: the symbol name, the kind (so a package-grain edge can be
// visibly labelled as such), and a file:line hint so the user can navigate
// to the call site.
type NodeInfo struct {
	Name     string
	Kind     string
	FilePath string
	Line     int
}

// ResolveCrossRepoNode best-effort resolves any cross-repo node_id (src or
// dst) to its symbol name + file location via eng_get_node, so the CLI can
// print "RunE in cmd/root.go:18 --CALLS--> greetlib.Greeter.Hello" instead
// of opaque hashes (extends,; rendering upgrade for
// ). Pass repoID/branch empty to let the daemon scan all (repo,
// branch) pairs — needed for inbound src nodes whose containing repo isn't
// on the response envelope. Returns the zero value on any error or empty
// result so a stuck remote repo never fails the primary output.
func ResolveCrossRepoNode(ctx context.Context, nodeID, repoID, branch string) NodeInfo {
	if nodeID == "" {
		return NodeInfo{}
	}
	var resp struct {
		Nodes []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start,omitempty"`
		} `json:"nodes"`
	}
	params := map[string]any{"node_id": nodeID}
	if repoID != "" {
		params["repo_id"] = repoID
	}
	if branch != "" {
		params["branch"] = branch
	}
	if err := mcpclient.Call(ctx, "eng_get_node", params, &resp); err != nil {
		return NodeInfo{}
	}
	if len(resp.Nodes) == 0 {
		return NodeInfo{}
	}
	n := resp.Nodes[0]
	return NodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, Line: n.LineStart}
}

// FormatCrossRepoNode renders one side of a cross-repo edge: the symbol
// name, optionally with a "package " prefix when the resolved node is a Go
// package (signalling the parser attributed the call to the package because
// it couldn't bind the specific function), and appended with "in
// file_path[:line]" when known. Falls back to a short-id slice of the raw
// node_id when nothing resolves so the row still has *some* identifier.
func FormatCrossRepoNode(info NodeInfo, fallbackID string) string {
	name := info.Name
	if name == "" {
		return repocmd.ShortRepoID(fallbackID)
	}
	if info.Kind == "package" {
		name = "package " + name
	}
	if info.FilePath != "" {
		if info.Line > 0 {
			return fmt.Sprintf("%s in %s:%d", name, info.FilePath, info.Line)
		}
		return fmt.Sprintf("%s in %s", name, info.FilePath)
	}
	return name
}
