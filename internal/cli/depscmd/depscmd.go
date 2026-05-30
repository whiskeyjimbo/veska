// Package depscmd holds the business logic behind the `veska deps` command
// family: listing the external modules a repo calls into (eng_list_dependencies)
// and indexing a vendored module's symbols into the graph. cmd/veska/deps.go is
// reduced to Cobra command construction whose RunE bodies delegate here,
// following the cmd = glue / logic-in-packages pattern established by
// reindexcmd, symbolcmd, graphcmd, and findingscmd (solov2-0omh).
package depscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// ResolveRepoFunc maps the caller's cwd to a repo_id via the running daemon,
// emitting a breadcrumb to errOut when it auto-scopes. It is injected by the
// Cobra layer (cmd/veska's autoResolveRepo) so depscmd does not depend on the
// daemon-discovery helpers directly.
type ResolveRepoFunc func(ctx context.Context, errOut io.Writer) string

// CallFunc issues one MCP request against the daemon. It mirrors mcpclient.Call
// so RunList can be unit-tested with a fake in place of the real socket client.
type CallFunc func(ctx context.Context, method string, params, out any) error

// ListParams bundles the inputs of RunList.
type ListParams struct {
	// RepoArg is the optional positional id-or-path selector ("" = none).
	RepoArg string
	// RepoID is the --repo flag value ("" = unset).
	RepoID      string
	Limit       int
	JSONOut     bool
	Out         io.Writer
	ErrOut      io.Writer
	ResolveRepo ResolveRepoFunc
	// Call defaults to mcpclient.Call when nil.
	Call CallFunc
}

// RunList wraps eng_list_dependencies: it resolves the target repo (positional
// id-or-path, then --repo, then cwd) and renders the modules the repo CALLS
// into, ranked by call-site count.
func RunList(ctx context.Context, p ListParams) error {
	call := p.Call
	if call == nil {
		call = mcpclient.Call
	}
	params := map[string]any{}
	switch {
	case p.RepoArg != "":
		// Accept the same identifiers `repo add` / `reindex` do (path,
		// repo_id, short_id) so the CLI is consistent (solov2-mtd0).
		rid, err := repocmd.ResolveRepoArg(ctx, p.RepoArg)
		if err != nil {
			return fmt.Errorf("deps: %w", err)
		}
		params["repo_id"] = rid
	case p.RepoID != "":
		params["repo_id"] = p.RepoID
	default:
		if rid := p.ResolveRepo(ctx, p.ErrOut); rid != "" {
			params["repo_id"] = rid
		}
	}

	var resp struct {
		Dependencies []struct {
			Module       string `json:"module"`
			Version      string `json:"version,omitempty"`
			Language     string `json:"language"`
			UsageCount   int    `json:"usage_count"`
			ImportCount  int    `json:"import_count,omitempty"`
			TopCallSites []struct {
				SrcNodeID  string `json:"src_node_id"`
				SymbolPath string `json:"symbol_path"`
			} `json:"top_call_sites"`
		} `json:"dependencies"`
	}
	if err := call(ctx, "eng_list_dependencies", params, &resp); err != nil {
		return fmt.Errorf("deps: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	if len(resp.Dependencies) == 0 {
		fmt.Fprintln(p.Out, "no external dependencies (or no calls into them yet — the graph fills in as files are promoted)")
		return nil
	}
	shown := resp.Dependencies
	truncated := 0
	if p.Limit > 0 && len(shown) > p.Limit {
		truncated = len(shown) - p.Limit
		shown = shown[:p.Limit]
	}
	tw := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODULE\tVERSION\tCALLS\tIMPORTS\tTOP_SYMBOLS")
	anyZeroCalls := false
	for _, d := range shown {
		var symbols strings.Builder
		for i, cs := range d.TopCallSites {
			if i > 0 {
				symbols.WriteString(", ")
			}
			symbols.WriteString(cs.SymbolPath)
		}
		// solov2-xok5: CALLS=0 with IMPORTS>0 almost always means the module
		// is used through chained selector expressions (e.g. cobra
		// `&cobra.Command{...}`, `yaml.Marshal`) that the parser attributes to
		// the package node, not the calling function. Without a marker, a
		// junior reads "CALLS=0" as "unused dep, safe to remove" — dangerous.
		// Tag the row with "*" and emit a footer explaining the suppression.
		callsCell := fmt.Sprintf("%d", d.UsageCount)
		if d.UsageCount == 0 && d.ImportCount > 0 {
			callsCell = "0 *"
			anyZeroCalls = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", d.Module, d.Version, callsCell, d.ImportCount, symbols.String())
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if truncated > 0 {
		fmt.Fprintf(p.Out, "... %d more (raise --limit to see all)\n", truncated)
	}
	if anyZeroCalls {
		fmt.Fprintln(p.Out, "* CALLS=0 with IMPORTS>0: call edges suppressed by chained_selectors_unresolved (cobra/yaml-style usage); the module is imported but its use is not 'unused'. Do not infer 'safe to remove' from CALLS=0 alone.")
	}
	return nil
}

// IndexParams bundles the inputs of RunIndex.
type IndexParams struct {
	ModulePath string
	// RepoID is the --repo flag value ("" = resolve from cwd).
	RepoID      string
	Out         io.Writer
	ErrOut      io.Writer
	ResolveRepo ResolveRepoFunc
}

// RunIndex scans <repoRoot>/vendor/<module-path> for .go files, parses them,
// and persists the nodes with external=1 so subsequent eng_find_symbol /
// eng_get_call_chain queries can see into vendored dependencies (solov2-bchl).
//
// Direct-write path: opens the local SQLite directly, mirroring the no-daemon
// fallback in `veska repo add`. The single-writer pool means the daemon should
// be stopped for this command.
func RunIndex(ctx context.Context, p IndexParams) error {
	db, closeFn, err := repocmd.OpenLocalDB()
	if err != nil {
		return fmt.Errorf("deps index: %w", err)
	}
	defer closeFn()

	// Resolve repo: --repo wins, else cwd-resolve, else error. cwd-resolve
	// needs the daemon to map cwd → repo, so on daemon-down systems the user
	// must pass --repo.
	repoID := p.RepoID
	if repoID == "" {
		repoID = p.ResolveRepo(ctx, p.ErrOut)
	}
	if repoID == "" {
		return errors.New("deps index: --repo <id> is required when no daemon is running to resolve cwd")
	}

	root, branch, err := repocmd.LookupRepoRootAndBranch(ctx, db, repoID)
	if err != nil {
		return fmt.Errorf("deps index: %w", err)
	}
	if root == "" {
		return fmt.Errorf("deps index: repo %s has no root_path; was it registered without a working tree?", repoID)
	}

	graph := sqlite.NewGraphRepo(db, db)
	svc, err := extindex.NewService(treesitter.NewGoParser(), graph,
		extindex.WithExternalRepoUpserter(graph))
	if err != nil {
		return fmt.Errorf("deps index: %w", err)
	}

	// solov2-izh6.7: refuse to index a vendored copy of a module that is
	// already a tracked registered repo — the synthetic ext:<module> row would
	// shadow the real repo's nodes and confuse cross-repo CALLS resolution.
	if existing, lookupErr := repocmd.FindTrackedRepoByModulePath(ctx, db, p.ModulePath); lookupErr == nil && existing != "" {
		return fmt.Errorf("deps index: module %s is already a tracked registered repo (%s); indexing its vendored copy would duplicate it. Re-run after `veska repo remove %s` if you really want the vendored snapshot instead, or just rely on the registered repo for cross-repo CALLS", p.ModulePath, existing, existing)
	}

	res, err := svc.IndexVendorModule(ctx, repoID, branch, root, p.ModulePath)
	if err != nil {
		if errors.Is(err, extindex.ErrModuleNotVendored) {
			return fmt.Errorf("deps index: %s is not vendored under %s/vendor/ — run `go mod vendor` first, or (phase 2) the module-cache path will cover non-vendored modules", p.ModulePath, root)
		}
		return fmt.Errorf("deps index: %w", err)
	}
	fmt.Fprintf(p.Out,
		"indexed %d node(s) across %d file(s) under %s/vendor/%s%s\n",
		res.Nodes, res.Files, root, p.ModulePath, skippedSuffix(res.Skipped))
	return nil
}

// skippedSuffix renders an optional " (N file(s) skipped)" suffix.
func skippedSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d file(s) skipped due to parse errors)", n)
}
