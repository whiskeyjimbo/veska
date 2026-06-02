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
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
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
		{family: f, tool: "eng_list_repos", bead: "solov2-p844", run: func(t *testing.T) {
			h := newHarness(t)
			res, rpcErr := h.Call("eng_list_repos", map[string]any{})
			if rpcErr != nil {
				t.Fatalf("eng_list_repos: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_list_repos: result type %T, want map[string]any", res)
			}
			views, ok := m["repos"].([]mcp.RepoView)
			if !ok {
				t.Fatalf("eng_list_repos: repos type %T, want []mcp.RepoView", m["repos"])
			}
			byID := map[string]mcp.RepoView{}
			for _, v := range views {
				byID[v.RepoID] = v
			}
			// Contains-all (not exact size) in case the harness seeds extra repos.
			for _, repoID := range []string{coverage.AlphaRepoID, coverage.BetaRepoID} {
				rv, present := byID[repoID]
				if !present {
					t.Errorf("eng_list_repos missing seeded repo %q (got %v)", repoID, byID)
					continue
				}
				if rv.RootPath != h.Root(repoID) {
					t.Errorf("%s root_path = %q, want %q", repoID, rv.RootPath, h.Root(repoID))
				}
				if rv.ActiveBranch != coverage.FixtureBranch {
					t.Errorf("%s active_branch = %q, want %q", repoID, rv.ActiveBranch, coverage.FixtureBranch)
				}
				if rv.Status != "promoted" {
					t.Errorf("%s status = %q, want %q", repoID, rv.Status, "promoted")
				}
				if rv.Kind != "tracked" {
					t.Errorf("%s kind = %q, want %q", repoID, rv.Kind, "tracked")
				}
				if rv.Aliases == nil {
					t.Errorf("%s aliases is nil, want non-nil ([])", repoID)
				}
			}
		}},
		{family: f, tool: "eng_get_repo", bead: "solov2-p4zv", run: func(t *testing.T) {
			h := newHarness(t)
			res, rpcErr := h.Call("eng_get_repo", map[string]any{"repo_id": coverage.AlphaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_get_repo: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_get_repo: result type %T, want map[string]any", res)
			}
			rv, ok := m["repo"].(mcp.RepoView)
			if !ok {
				t.Fatalf("eng_get_repo: repo type %T, want mcp.RepoView", m["repo"])
			}
			if rv.RepoID != coverage.AlphaRepoID {
				t.Errorf("repo_id = %q, want %q", rv.RepoID, coverage.AlphaRepoID)
			}
			if want := h.Root(coverage.AlphaRepoID); rv.RootPath != want {
				t.Errorf("root_path = %q, want %q", rv.RootPath, want)
			}
			if rv.ActiveBranch != "main" {
				t.Errorf("active_branch = %q, want %q", rv.ActiveBranch, "main")
			}
			if rv.Status != "promoted" {
				t.Errorf("status = %q, want %q", rv.Status, "promoted")
			}
			// Unknown repo_id is a domain miss surfaced as CodeNotFound.
			_, nfErr := h.Call("eng_get_repo", map[string]any{"repo_id": "nonexistent-repo"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown repo_id: got %v, want CodeNotFound", nfErr)
			}
		}},
		{family: f, tool: "eng_get_current_repo", bead: "solov2-mhfa", run: func(t *testing.T) {
			h := newHarness(t)
			// Two repos are seeded, so empty cwd is ambiguous: must pass a cwd
			// under Alpha's root. A real subdir exercises the HasPrefix match.
			cwd := filepath.Join(h.Root(coverage.AlphaRepoID), "metric")
			res, rpcErr := h.Call("eng_get_current_repo", map[string]any{"cwd": cwd})
			if rpcErr != nil {
				t.Fatalf("eng_get_current_repo: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_get_current_repo: result type %T, want map[string]any", res)
			}
			rv, ok := m["repo"].(mcp.RepoView)
			if !ok {
				t.Fatalf("eng_get_current_repo: repo type %T, want mcp.RepoView", m["repo"])
			}
			if rv.RepoID != coverage.AlphaRepoID {
				t.Errorf("repo_id = %q, want %q", rv.RepoID, coverage.AlphaRepoID)
			}
			if want := h.Root(coverage.AlphaRepoID); rv.RootPath != want {
				t.Errorf("root_path = %q, want %q", rv.RootPath, want)
			}
			// cwd matching no registered repo root is surfaced as CodeInvalidParams.
			_, nfErr := h.Call("eng_get_current_repo", map[string]any{"cwd": "/nonexistent/path/xyz"})
			if nfErr == nil || nfErr.Code != mcp.CodeInvalidParams {
				t.Fatalf("unmatched cwd: got %v, want CodeInvalidParams", nfErr)
			}
		}},
		{family: f, tool: "eng_get_status", bead: "solov2-mxbd", run: func(t *testing.T) {
			h := newHarness(t)
			res, rpcErr := h.Call("eng_get_status", map[string]any{})
			if rpcErr != nil {
				t.Fatalf("eng_get_status: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_get_status: result type %T, want map[string]any", res)
			}
			if sv, ok := m["schema_version"].(int); !ok || sv <= 0 {
				t.Errorf("schema_version = %v (%T), want positive int", m["schema_version"], m["schema_version"])
			}
			if rc, ok := m["repo_count"].(int); !ok || rc != 2 {
				t.Errorf("repo_count = %v (%T), want int 2 (Alpha+Beta)", m["repo_count"], m["repo_count"])
			}
			if pe, ok := m["pending_embeds"].(int); !ok || pe != 0 {
				t.Errorf("pending_embeds = %v (%T), want int 0 (drained)", m["pending_embeds"], m["pending_embeds"])
			}
			if st, _ := m["status"].(string); st != "ok" {
				t.Errorf("status = %q, want \"ok\" (no pending embeds)", st)
			}
			if dr, ok := m["degraded_reasons"].([]string); !ok || len(dr) != 0 {
				t.Errorf("degraded_reasons = %v (%T), want empty []string", m["degraded_reasons"], m["degraded_reasons"])
			}
		}},
		{family: f, tool: "eng_get_config", bead: "solov2-f11k", run: func(t *testing.T) {
			h := newHarness(t)
			res, rpcErr := h.Call("eng_get_config", map[string]any{})
			if rpcErr != nil {
				t.Fatalf("eng_get_config: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_get_config: result type %T, want map[string]any", res)
			}
			// Stable derived facts: payload-shape version is the literal 1 (int,
			// no JSON round-trip), and degraded_reasons is an empty []string.
			if csv, ok := m["config_schema_version"].(int); !ok || csv != 1 {
				t.Errorf("config_schema_version = %v (%T), want int 1", m["config_schema_version"], m["config_schema_version"])
			}
			if dr, ok := m["degraded_reasons"].([]string); !ok || len(dr) != 0 {
				t.Errorf("degraded_reasons = %v (%T), want empty []string", m["degraded_reasons"], m["degraded_reasons"])
			}
			// Under the harness's empty Config{}, path/url/model fields are empty
			// and the embedder marker is absent, so assert key PRESENCE (the
			// well-formed shape) rather than operator-specific values.
			for _, k := range []string{
				"veska_home", "sqlite_path", "cli_sock", "mcp_sock", "vector_backend",
				"embedder", "ollama_url", "embed_model", "config_schema_version", "degraded_reasons",
			} {
				if _, present := m[k]; !present {
					t.Errorf("eng_get_config missing key %q (got %v)", k, m)
				}
			}
		}},
		{family: f, tool: "eng_set_repo_alias", bead: "solov2-awb9"},
		{family: f, tool: "eng_remove_repo_alias", bead: "solov2-ffvx"},
		{family: f, tool: "eng_find_owner", bead: "solov2-a6ud"},
	}
}

func findingFamily() []coverageTool {
	const f = "finding"
	return []coverageTool{
		{family: f, tool: "eng_list_findings", bead: "solov2-f6xk", run: func(t *testing.T) {
			h := newHarness(t)
			// findingRow is unexported in package mcp, so round-trip the result
			// through JSON into a local shape rather than type-asserting it.
			type frow struct {
				FindingID string `json:"finding_id"`
				Rule      string `json:"rule"`
				Severity  string `json:"severity"`
				State     string `json:"state"`
				Message   string `json:"message"`
				ActorKind string `json:"actor_kind"`
			}
			list := func(params map[string]any) map[string]frow {
				res, rpcErr := h.Call("eng_list_findings", params)
				if rpcErr != nil {
					t.Fatalf("eng_list_findings %v: %v", params, rpcErr)
				}
				var out struct {
					Findings []frow `json:"findings"`
				}
				b, _ := json.Marshal(res)
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatalf("decode findings: %v", err)
				}
				bySeed := map[string]frow{}
				for _, fr := range out.Findings {
					bySeed[fr.FindingID] = fr
				}
				return bySeed
			}

			// rule="complexity" filters out the parser-emitted todo findings, so
			// the default (state=open) Alpha list collapses to exactly the seeded
			// complexity finding.
			alpha := list(map[string]any{"repo_id": coverage.AlphaRepoID, "rule": "complexity"})
			if len(alpha) != 1 {
				t.Fatalf("Alpha complexity findings = %v, want exactly 1", alpha)
			}
			fr, ok := alpha["seed-finding-0"]
			if !ok {
				t.Fatalf("Alpha complexity set %v missing seed-finding-0", alpha)
			}
			if fr.Rule != "complexity" || fr.Severity != "warn" || fr.State != "open" || fr.ActorKind != "agent" {
				t.Errorf("seed-finding-0 = %+v, want rule=complexity severity=warn state=open actor_kind=agent", fr)
			}
			if !strings.Contains(fr.Message, "high cyclomatic") {
				t.Errorf("seed-finding-0 message %q missing %q", fr.Message, "high cyclomatic")
			}

			// state=closed (default is open) surfaces the seeded closed style
			// finding on Beta.
			beta := list(map[string]any{"repo_id": coverage.BetaRepoID, "state": "closed", "rule": "style"})
			br, ok := beta["seed-finding-1"]
			if !ok {
				t.Fatalf("Beta closed style set %v missing seed-finding-1", beta)
			}
			if br.Rule != "style" || br.Severity != "info" || br.State != "closed" {
				t.Errorf("seed-finding-1 = %+v, want rule=style severity=info state=closed", br)
			}
		}},
		{family: f, tool: "eng_get_finding", bead: "solov2-y69v", run: func(t *testing.T) {
			h := newHarness(t)
			// findingRow is unexported in package mcp and the handler returns
			// map[string]any{"finding": findingRow}; round-trip via JSON.
			type frow struct {
				FindingID string `json:"finding_id"`
				Rule      string `json:"rule"`
				Severity  string `json:"severity"`
				State     string `json:"state"`
				Message   string `json:"message"`
				ActorKind string `json:"actor_kind"`
			}
			res, rpcErr := h.Call("eng_get_finding", map[string]any{
				"finding_id": "seed-finding-0", "branch": coverage.FixtureBranch,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_finding: %v", rpcErr)
			}
			var got struct {
				Finding frow `json:"finding"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode finding: %v", err)
			}
			fr := got.Finding
			if fr.FindingID != "seed-finding-0" || fr.Rule != "complexity" || fr.Severity != "warn" || fr.State != "open" || fr.ActorKind != "agent" {
				t.Errorf("seed-finding-0 = %+v, want id=seed-finding-0 rule=complexity severity=warn state=open actor_kind=agent", fr)
			}
			if !strings.Contains(fr.Message, "high cyclomatic") {
				t.Errorf("seed-finding-0 message %q missing %q", fr.Message, "high cyclomatic")
			}
			// Unknown finding_id resolves to zero rows -> CodeNotFound.
			_, nfErr := h.Call("eng_get_finding", map[string]any{"finding_id": "seed-finding-does-not-exist"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown finding_id: got %v, want CodeNotFound", nfErr)
			}
			// repo_id mismatch (Alpha finding scoped to Beta) -> CodeNotFound.
			_, mmErr := h.Call("eng_get_finding", map[string]any{
				"finding_id": "seed-finding-0", "repo_id": coverage.BetaRepoID,
			})
			if mmErr == nil || mmErr.Code != mcp.CodeNotFound {
				t.Fatalf("repo_id mismatch: got %v, want CodeNotFound", mmErr)
			}
		}},
		{family: f, tool: "eng_close_finding", bead: "solov2-tvid"},
		{family: f, tool: "eng_reopen_finding", bead: "solov2-ifne"},
		{family: f, tool: "eng_find_todos", bead: "solov2-rrz1", run: func(t *testing.T) {
			h := newHarness(t)
			// Per-repo: assert the returned file_path SET equals the manifest's
			// TodoFacts filtered by RepoID, and the Text appears in the message.
			assertRepoTodos := func(repoID string) {
				res, rpcErr := h.Call("eng_find_todos", map[string]any{"repo_id": repoID})
				if rpcErr != nil {
					t.Fatalf("eng_find_todos %s: %v", repoID, rpcErr)
				}
				resp, ok := res.(mcp.TodosResponse)
				if !ok {
					t.Fatalf("eng_find_todos: result type %T, want mcp.TodosResponse", res)
				}
				want := map[string]string{} // rel path -> manifest Text
				for _, td := range coverage.Manifest().Todos {
					if td.RepoID == repoID {
						want[td.RelPath] = td.Text
					}
				}
				got := map[string]bool{}
				for _, td := range resp.Todos {
					got[td.FilePath] = true
					if text, ok := want[td.FilePath]; ok && !strings.Contains(td.Message, text) {
						t.Errorf("todo %s: message %q missing manifest text %q", td.FilePath, td.Message, text)
					}
				}
				for rel := range want {
					if !got[rel] {
						t.Errorf("manifest todo %q missing from eng_find_todos output for %s", rel, repoID)
					}
				}
			}
			assertRepoTodos(coverage.AlphaRepoID)
			assertRepoTodos(coverage.BetaRepoID)
		}},
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
		{family: f, tool: "eng_get_call_chain", bead: "solov2-zk8c", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			seed := h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/deviation.go", Kind: domain.KindFunction, Name: "StandardDeviation"})

			res, rpcErr := h.Call("eng_get_call_chain", map[string]any{
				"node_id": string(seed), "repo_id": repoID, "depth": 3,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_call_chain: %v", rpcErr)
			}
			// callChainResponse + its node/edge DTOs are unexported in package
			// mcp, so round-trip the result through JSON into a local shape with
			// the same tags rather than type-asserting an unreachable type.
			var resp struct {
				Nodes []struct {
					NodeID string `json:"node_id"`
				} `json:"nodes"`
				Edges []struct {
					Src  string `json:"src_node_id"`
					Dst  string `json:"dst_node_id"`
					Kind string `json:"kind"`
				} `json:"edges"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &resp); err != nil {
				t.Fatalf("decode call chain: %v", err)
			}

			// Expected CALLS edges = the manifest's frozen Alpha CALLS facts (all
			// reachable from StandardDeviation within depth 3). Nodes derive from
			// those edges' endpoints, so no second hardcoded resolve list.
			wantEdges, wantNodes := map[string]bool{}, map[string]bool{}
			for _, e := range coverage.Manifest().Edges {
				if e.Kind != domain.EdgeCalls || e.RepoID != repoID {
					continue
				}
				src, dst := h.ResolveID(repoID, e.Src), h.ResolveID(repoID, e.Dst)
				wantEdges[string(src)+"\x00"+string(dst)+"\x00"+string(e.Kind)] = true
				// resultNodes holds callees only (the seed is marked visited but
				// never appended), so assert presence of edge endpoints other
				// than the seed.
				if src != seed {
					wantNodes[string(src)] = true
				}
				if dst != seed {
					wantNodes[string(dst)] = true
				}
			}
			gotEdges, gotNodes := map[string]bool{}, map[string]bool{}
			for _, e := range resp.Edges {
				gotEdges[e.Src+"\x00"+e.Dst+"\x00"+e.Kind] = true
			}
			for _, n := range resp.Nodes {
				gotNodes[n.NodeID] = true
			}
			for k := range wantEdges {
				if !gotEdges[k] {
					t.Errorf("CALLS edge %q missing from call chain", k)
				}
			}
			for id := range wantNodes {
				if !gotNodes[id] {
					t.Errorf("reachable node %q missing from call chain", id)
				}
			}
		}},
		{family: f, tool: "eng_get_file_nodes", bead: "solov2-2zlq", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			const file = "metric/series.go"

			res, rpcErr := h.Call("eng_get_file_nodes", map[string]any{
				"file_path": file, "repo_id": repoID,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_file_nodes: %v", rpcErr)
			}
			resp, ok := res.(mcp.GraphResponse)
			if !ok {
				t.Fatalf("eng_get_file_nodes: result type %T, want mcp.GraphResponse", res)
			}
			// Expected manifest facts for this file (chunk nodes are excluded
			// from the manifest by design — their names are volatile line ranges).
			wantPath := filepath.Join(h.Root(repoID), filepath.FromSlash(file))
			var want []string
			for _, k := range coverage.Manifest().Nodes {
				if k.Path == file {
					want = append(want, string(k.Kind)+"\x00"+k.Name)
				}
			}
			// (a) every manifest node is PRESENT in the returned set, with the
			// manifest's path fact; chunk nodes in output are ignored here.
			got := map[string]bool{}
			var nonChunk []string
			for _, n := range resp.Nodes {
				if n.Kind == string(domain.KindChunk) {
					continue
				}
				if n.FilePath != wantPath {
					t.Errorf("node %q file_path = %q, want %q", n.Name, n.FilePath, wantPath)
				}
				key := n.Kind + "\x00" + n.Name
				got[key] = true
				nonChunk = append(nonChunk, key)
			}
			for _, w := range want {
				if !got[w] {
					t.Errorf("manifest node %q missing from eng_get_file_nodes output", w)
				}
			}
			// (b) chunk-filtered output equals the manifest set (normalized SET).
			sort.Strings(want)
			sort.Strings(nonChunk)
			if !reflect.DeepEqual(nonChunk, want) {
				t.Errorf("non-chunk node set = %v, want %v", nonChunk, want)
			}
		}},
		{family: f, tool: "eng_find_related", bead: "solov2-d217"},
	}
}

func blastFamily() []coverageTool {
	const f = "blast"
	return []coverageTool{
		{family: f, tool: "eng_get_blast_radius", bead: "solov2-6ups", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			// Seed: computeMean. In CALLERS direction the affected set is its
			// direct caller (ComputeVariance) and its transitive caller
			// (StandardDeviation → ComputeVariance → computeMean).
			seed := h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "computeMean"})

			res, rpcErr := h.Call("eng_get_blast_radius", map[string]any{
				"node_id": string(seed), "repo_id": repoID,
				"direction": "callers", "max_depth": 5,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_blast_radius: %v", rpcErr)
			}
			resp, ok := res.(mcp.BlastResponse)
			if !ok {
				t.Fatalf("eng_get_blast_radius: result type %T, want mcp.BlastResponse", res)
			}
			// The seed itself rides along (depth 0), so assert contains-all on the
			// transitively-affected set rather than an exact size.
			want := map[string]bool{
				string(h.ResolveID(repoID, coverage.NodeKey{
					Path: "metric/series.go", Kind: domain.KindFunction, Name: "ComputeVariance"})): true,
				string(h.ResolveID(repoID, coverage.NodeKey{
					Path: "metric/deviation.go", Kind: domain.KindFunction, Name: "StandardDeviation"})): true,
			}
			got := map[string]bool{}
			for _, e := range resp.Entries {
				got[e.NodeID] = true
			}
			for id := range want {
				if !got[id] {
					t.Errorf("affected caller %q missing from blast radius", id)
				}
			}
		}},
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
		{family: f, tool: "eng_find_symbol", bead: "solov2-f6lt", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.BetaRepoID
			key := coverage.NodeKey{Path: "main.go", Kind: domain.KindFunction, Name: "main"}
			declID := h.ResolveID(repoID, key)

			res, rpcErr := h.Call("eng_find_symbol", map[string]any{
				"symbol": key.Name, "repo_id": repoID,
			})
			if rpcErr != nil {
				t.Fatalf("eng_find_symbol: %v", rpcErr)
			}
			resp, ok := res.(mcp.GraphResponse)
			if !ok {
				t.Fatalf("eng_find_symbol: result type %T, want mcp.GraphResponse", res)
			}
			// "main" matches both the function and the per-file package node, so the
			// declaration-before-container ordering contract is actually exercised.
			if len(resp.Nodes) < 2 {
				t.Fatalf("got %d nodes for %q, want >=2 (function + package) to test ordering", len(resp.Nodes), key.Name)
			}
			// Ordering contract: nodes[0] is the declaration, never a container.
			containers := map[string]bool{
				string(domain.KindPackage): true, string(domain.KindFile): true,
				string(domain.KindModule): true, string(domain.KindChunk): true,
			}
			if containers[resp.Nodes[0].Kind] {
				t.Errorf("nodes[0].kind = %q is a container; declaration must sort first", resp.Nodes[0].Kind)
			}
			if resp.Nodes[0].NodeID != string(declID) {
				t.Errorf("nodes[0].node_id = %q, want declaration %q", resp.Nodes[0].NodeID, string(declID))
			}
			// Set CONTAINS the declaration node with the manifest's graph facts.
			wantPath := filepath.Join(h.Root(repoID), key.Path)
			found := false
			for _, n := range resp.Nodes {
				if n.NodeID == string(declID) {
					found = true
					if n.Name != key.Name || n.Kind != string(key.Kind) || n.FilePath != wantPath {
						t.Errorf("decl node = {%q %q %q}, want {%q %q %q}",
							n.Name, n.Kind, n.FilePath, key.Name, string(key.Kind), wantPath)
					}
				}
			}
			if !found {
				t.Errorf("returned set missing declaration node %q", string(declID))
			}
			// kind filter narrows results: package-only excludes the function decl.
			fres, frpcErr := h.Call("eng_find_symbol", map[string]any{
				"symbol": key.Name, "repo_id": repoID, "kind": string(domain.KindPackage),
			})
			if frpcErr != nil {
				t.Fatalf("eng_find_symbol (kind filter): %v", frpcErr)
			}
			fresp := fres.(mcp.GraphResponse)
			for _, n := range fresp.Nodes {
				if n.Kind != string(domain.KindPackage) {
					t.Errorf("kind=package filter returned %q node", n.Kind)
				}
			}
		}},
	}
}

func wikiFamily() []coverageTool {
	const f = "wiki"
	return []coverageTool{
		{family: f, tool: "eng_get_hot_zone", bead: "solov2-17kd"},
		{family: f, tool: "eng_get_entry_points", bead: "solov2-tqda", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.BetaRepoID
			res, rpcErr := h.Call("eng_get_entry_points", map[string]any{"repo_id": repoID})
			if rpcErr != nil {
				t.Fatalf("eng_get_entry_points: %v", rpcErr)
			}
			resp, ok := res.(mcp.EntryPointsResponse)
			if !ok {
				t.Fatalf("eng_get_entry_points: result type %T, want mcp.EntryPointsResponse", res)
			}
			// Selection runs fan-in + hidden gates (e.g. Alpha's exported
			// ComputeVariance is dropped despite inbound>=1), so assert only the
			// manifest's frozen entry points are PRESENT — not an exact set.
			// EntryPoint.FilePath is absolute on the wire (mirrors sibling DTOs).
			got := map[string]bool{}
			for _, e := range resp.EntryPoints {
				got[e.SymbolName+"\x00"+e.FilePath] = true
			}
			for _, ep := range coverage.Manifest().EntryPoints {
				if ep.RepoID != repoID {
					continue
				}
				want := ep.Node.Name + "\x00" + filepath.Join(h.Root(repoID), ep.Node.Path)
				if !got[want] {
					t.Errorf("manifest entry point %q (%s) missing from output", ep.Node.Name, ep.Node.Path)
				}
			}
		}},
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
		{family: f, tool: "eng_list_dependencies", bead: "solov2-1zhc", run: func(t *testing.T) {
			h := newHarness(t)
			// Beta CALLS into modalpha/metric (the one genuine cross-module dep);
			// modalpha imports nothing external. Assert CONTAINS, not exact set:
			// per solov2-tb74 isExternalModulePath fails to subtract modbeta's OWN
			// module path, so "example.com/modbeta/widget" may LEAK in. A future
			// fix removing the leak keeps contains-all valid.
			res, rpcErr := h.Call("eng_list_dependencies", map[string]any{"repo_id": coverage.BetaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_list_dependencies: %v", rpcErr)
			}
			resp, ok := res.(dependencies.Result)
			if !ok {
				t.Fatalf("eng_list_dependencies: result type %T, want dependencies.Result", res)
			}
			got := map[string]dependencies.Dependency{}
			for _, d := range resp.Dependencies {
				got[d.Module] = d
			}
			const wantMod = "example.com/modalpha/metric"
			dep, present := got[wantMod]
			if !present {
				t.Fatalf("Beta deps %v missing %q", resp.Dependencies, wantMod)
			}
			if dep.UsageCount < 1 {
				t.Errorf("%s usage_count = %d, want >=1", wantMod, dep.UsageCount)
			}
			// The one genuine call site is Badge.RenderBadge -> ComputeVariance.
			hasComputeVariance := false
			for _, cs := range dep.TopCallSites {
				if strings.Contains(cs.SymbolPath, "ComputeVariance") {
					hasComputeVariance = true
				}
			}
			if !hasComputeVariance {
				t.Errorf("%s top_call_sites %v missing ComputeVariance", wantMod, dep.TopCallSites)
			}
			// modalpha imports nothing external: Alpha has no cross-module deps.
			ares, arpcErr := h.Call("eng_list_dependencies", map[string]any{"repo_id": coverage.AlphaRepoID})
			if arpcErr != nil {
				t.Fatalf("eng_list_dependencies (alpha): %v", arpcErr)
			}
			if deps := ares.(dependencies.Result).Dependencies; len(deps) != 0 {
				t.Errorf("Alpha deps = %v, want empty", deps)
			}
		}},
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
