package daemon

// TestToolCoverage is the parent skeleton (solov2-ti9x) for the 40 per-tool MCP
// coverage beads. It is a per-FAMILY subtest tree where every one of the 40
// tools is a PENDING leaf: each leaf currently calls t.Skip with its owning
// bead ID and makes ZERO real assertions. A per-tool bead replaces ONLY its own
// leaf's body — it does not touch the table or any sibling.
//
// HOW A TOOL BEAD PLUGS IN (exact pattern):
//
//	Find the entry in coverageTools for your tool (e.g. eng_get_node /
//	solov2-w775) and replace its run func. Inside, build a harness and call:
//
//	    func(t *testing.T) {
//	        h := newHarness(t)
//	        id := h.ResolveID(coverage.BetaRepoID, coverage.NodeKey{
//	            Path: "main.go", Kind: domain.KindFunction, Name: "main"})
//	        res, rpcErr := h.Call("eng_get_node", map[string]any{"node_id": string(id)})
//	        if rpcErr != nil { t.Fatalf("eng_get_node: %v", rpcErr) }
//	        // ... assert on res against coverage.Manifest() facts ...
//	    }
//
//	For a MUTATING tool just do the same — newHarness gives a fresh isolated
//	DB + vector store, so the mutation cannot leak to any other subtest. For a
//	TASK tool (eng_set_active_task / eng_get_active_task / eng_get_task_history)
//	construct the harness with the opt-in: newHarness(t, WithTaskTools()).
//
// The leaf must STOP calling t.Skip once it asserts. The completeness guard
// (TestToolCoverageCompleteness) keeps the table in lock-step with the live
// tool surface so no tool is silently uncovered.

import (
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
)

// coverageTool is one row of the coverage table: which family the tool belongs
// to, the tool name, the owning bead, whether it is one of the parked task
// tools (so the leaf must build the harness with WithTaskTools), and the run
// func a bead replaces. run is nil until a bead fills it; a nil run yields a
// PENDING skip keyed on the bead ID.
type coverageTool struct {
	family string
	tool   string
	bead   string
	task   bool // parked task tool: covered only via newHarness(t, WithTaskTools())
	run    func(t *testing.T)
}

// coverageTools is the single source of truth: the 40 MCP tools grouped by
// family. The completeness guard asserts this set equals the live registry's
// 37 tools + the 3 opt-in task tools. Keep families small and intent-named.
func coverageTools() []coverageTool {
	var out []coverageTool
	out = append(out, repoFamily()...)
	out = append(out, findingFamily()...)
	out = append(out, suppressionFamily()...)
	out = append(out, taskFamily()...)
	out = append(out, graphFamily()...)
	out = append(out, blastFamily()...)
	out = append(out, searchFamily()...)
	out = append(out, symbolFamily()...)
	out = append(out, wikiFamily()...)
	out = append(out, contextFamily()...)
	out = append(out, dependencyFamily()...)
	out = append(out, cloneFamily()...)
	out = append(out, changedSymbolsFamily()...)
	out = append(out, promotionFamily()...)
	return out
}

func repoFamily() []coverageTool {
	const f = "repo"
	return []coverageTool{
		{family: f, tool: "eng_add_repo", bead: "solov2-ieuu"},
		{family: f, tool: "eng_remove_repo", bead: "solov2-e6xw"},
		{family: f, tool: "eng_list_repos", bead: "solov2-p844"},
		{family: f, tool: "eng_get_repo", bead: "solov2-p4zv"},
		{family: f, tool: "eng_get_current_repo", bead: "solov2-mhfa"},
		{family: f, tool: "eng_get_status", bead: "solov2-mxbd"},
		{family: f, tool: "eng_get_config", bead: "solov2-f11k"},
		{family: f, tool: "eng_set_repo_alias", bead: "solov2-awb9"},
		{family: f, tool: "eng_remove_repo_alias", bead: "solov2-ffvx"},
		{family: f, tool: "eng_find_owner", bead: "solov2-a6ud"},
	}
}

func findingFamily() []coverageTool {
	const f = "finding"
	return []coverageTool{
		{family: f, tool: "eng_list_findings", bead: "solov2-f6xk"},
		{family: f, tool: "eng_get_finding", bead: "solov2-y69v"},
		{family: f, tool: "eng_close_finding", bead: "solov2-tvid"},
		{family: f, tool: "eng_reopen_finding", bead: "solov2-ifne"},
		{family: f, tool: "eng_find_todos", bead: "solov2-rrz1"},
	}
}

func suppressionFamily() []coverageTool {
	const f = "suppression"
	return []coverageTool{
		{family: f, tool: "eng_suppress_finding", bead: "solov2-uq5t"},
		{family: f, tool: "eng_get_suppression", bead: "solov2-9735"},
		{family: f, tool: "eng_list_suppressions", bead: "solov2-avb5"},
		{family: f, tool: "eng_close_suppression", bead: "solov2-lmhn"},
	}
}

// taskFamily holds the 3 PARKED task tools — covered only via the WithTaskTools
// opt-in (production registers 37, not 40). task=true marks the opt-in need.
func taskFamily() []coverageTool {
	const f = "task"
	return []coverageTool{
		{family: f, tool: "eng_set_active_task", bead: "solov2-orrj", task: true},
		{family: f, tool: "eng_get_active_task", bead: "solov2-0cgj", task: true},
		{family: f, tool: "eng_get_task_history", bead: "solov2-58sw", task: true},
	}
}

func graphFamily() []coverageTool {
	const f = "graph"
	return []coverageTool{
		{family: f, tool: "eng_get_node", bead: "solov2-w775", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.BetaRepoID
			key := coverage.NodeKey{Path: "main.go", Kind: domain.KindFunction, Name: "main"}
			id := h.ResolveID(repoID, key)

			res, rpcErr := h.Call("eng_get_node", map[string]any{
				"node_id": string(id), "repo_id": repoID,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_node: %v", rpcErr)
			}
			resp, ok := res.(mcp.GraphResponse)
			if !ok {
				t.Fatalf("eng_get_node: result type %T, want mcp.GraphResponse", res)
			}
			if len(resp.Nodes) != 1 {
				t.Fatalf("eng_get_node: got %d nodes, want exactly 1", len(resp.Nodes))
			}
			// Single-node tool: no list ordering to normalize. Assert the one
			// returned node carries the graph facts the manifest records.
			n := resp.Nodes[0]
			if n.NodeID != string(id) {
				t.Errorf("node_id = %q, want %q", n.NodeID, string(id))
			}
			if n.Name != key.Name {
				t.Errorf("name = %q, want %q", n.Name, key.Name)
			}
			if n.Kind != string(key.Kind) {
				t.Errorf("kind = %q, want %q", n.Kind, string(key.Kind))
			}
			// Node paths are stored absolute; manifest Path is repo-relative.
			if want := filepath.Join(h.Root(repoID), key.Path); n.FilePath != want {
				t.Errorf("file_path = %q, want %q", n.FilePath, want)
			}

			// Not-found is a domain error surfaced as CodeNotFound, not a marshal error.
			_, nfErr := h.Call("eng_get_node", map[string]any{
				"node_id": "deadbeef-not-a-real-node", "repo_id": repoID,
			})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("bogus node_id: got %v, want CodeNotFound", nfErr)
			}
		}},
		{family: f, tool: "eng_get_call_chain", bead: "solov2-zk8c"},
		{family: f, tool: "eng_get_file_nodes", bead: "solov2-2zlq"},
		{family: f, tool: "eng_find_related", bead: "solov2-d217"},
	}
}

func blastFamily() []coverageTool {
	const f = "blast"
	return []coverageTool{
		{family: f, tool: "eng_get_blast_radius", bead: "solov2-6ups"},
		{family: f, tool: "eng_get_dirty_blast_radius", bead: "solov2-1sya"},
		{family: f, tool: "eng_get_diff_blast_radius", bead: "solov2-56c8"},
	}
}

func searchFamily() []coverageTool {
	const f = "search"
	return []coverageTool{
		{family: f, tool: "eng_search_semantic", bead: "solov2-4g0h"},
		{family: f, tool: "eng_search_similar", bead: "solov2-r1ue"},
	}
}

func symbolFamily() []coverageTool {
	const f = "symbol"
	return []coverageTool{
		{family: f, tool: "eng_find_symbol", bead: "solov2-f6lt"},
	}
}

func wikiFamily() []coverageTool {
	const f = "wiki"
	return []coverageTool{
		{family: f, tool: "eng_get_hot_zone", bead: "solov2-17kd"},
		{family: f, tool: "eng_get_entry_points", bead: "solov2-tqda"},
	}
}

func contextFamily() []coverageTool {
	const f = "context"
	return []coverageTool{
		{family: f, tool: "eng_get_context_pack", bead: "solov2-xjjk"},
	}
}

func dependencyFamily() []coverageTool {
	const f = "dependency"
	return []coverageTool{
		{family: f, tool: "eng_list_dependencies", bead: "solov2-1zhc"},
	}
}

func cloneFamily() []coverageTool {
	const f = "clone"
	return []coverageTool{
		{family: f, tool: "eng_find_clones", bead: "solov2-8jfs"},
	}
}

func changedSymbolsFamily() []coverageTool {
	const f = "changed_symbols"
	return []coverageTool{
		{family: f, tool: "eng_find_changed_symbols", bead: "solov2-m2wp"},
	}
}

func promotionFamily() []coverageTool {
	const f = "promotion"
	return []coverageTool{
		{family: f, tool: "eng_promote_repo", bead: "solov2-0buu"},
		{family: f, tool: "eng_reindex_repo", bead: "solov2-extk"},
	}
}

// TestToolCoverage runs the per-family subtest tree. Each tool is a leaf:
// either a real (bead-supplied) assertion or a PENDING skip. The skip MUST name
// the owning bead so `go test -run TestToolCoverage -v` doubles as a checklist.
func TestToolCoverage(t *testing.T) {
	for _, ct := range coverageTools() {
		ct := ct
		t.Run(ct.family+"/"+ct.tool, func(t *testing.T) {
			if ct.run == nil {
				t.Skipf("pending: %s — replace this leaf's run func with real assertions", ct.bead)
				return
			}
			ct.run(t)
		})
	}
}
