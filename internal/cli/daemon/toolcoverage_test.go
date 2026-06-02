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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
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
		{family: f, tool: "eng_add_repo", bead: "solov2-ieuu", run: func(t *testing.T) {
			h := newHarness(t)
			repoIDSet := func() map[string]bool {
				res, rpcErr := h.Call("eng_list_repos", map[string]any{})
				if rpcErr != nil {
					t.Fatalf("eng_list_repos: %v", rpcErr)
				}
				ids := map[string]bool{}
				for _, v := range res.(map[string]any)["repos"].([]mcp.RepoView) {
					ids[v.RepoID] = true
				}
				return ids
			}
			// repo.Add walks up for a .git work-tree marker; create one (no git binary needed).
			newRepo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(newRepo, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			// BEFORE: the two fixtures are present; the new repo is not.
			before := repoIDSet()
			if !before[coverage.AlphaRepoID] || !before[coverage.BetaRepoID] {
				t.Fatalf("before add: set %v missing a seeded fixture", before)
			}
			// MUTATE: register the brand-new path.
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": newRepo})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			m := res.(map[string]any)
			newID, _ := m["repo_id"].(string)
			if newID == "" {
				t.Fatalf("add returned empty repo_id (got %v)", m)
			}
			if before[newID] {
				t.Fatalf("repo_id %q was already registered before add", newID)
			}
			if m["already_registered"] != false {
				t.Errorf("already_registered = %v, want false", m["already_registered"])
			}
			// AFTER: the returned repo_id now appears in eng_list_repos.
			if !repoIDSet()[newID] {
				t.Errorf("after add: eng_list_repos missing new repo %q", newID)
			}
		}},
		{family: f, tool: "eng_remove_repo", bead: "solov2-e6xw", run: func(t *testing.T) {
			h := newHarness(t)
			repoIDSet := func() map[string]bool {
				res, rpcErr := h.Call("eng_list_repos", map[string]any{})
				if rpcErr != nil {
					t.Fatalf("eng_list_repos: %v", rpcErr)
				}
				ids := map[string]bool{}
				for _, v := range res.(map[string]any)["repos"].([]mcp.RepoView) {
					ids[v.RepoID] = true
				}
				return ids
			}
			// repo.Add walks up for a .git work-tree marker; create one (no git binary needed).
			newRepo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(newRepo, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": newRepo})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			newID, _ := res.(map[string]any)["repo_id"].(string)
			// BEFORE: the freshly added repo is present.
			if !repoIDSet()[newID] {
				t.Fatalf("before remove: eng_list_repos missing added repo %q", newID)
			}
			// MUTATE: unregister it.
			res, rpcErr = h.Call("eng_remove_repo", map[string]any{"repo_id": newID})
			if rpcErr != nil {
				t.Fatalf("eng_remove_repo: %v", rpcErr)
			}
			m := res.(map[string]any)
			if m["removed"] != true || m["repo_id"] != newID {
				t.Errorf("remove returned %v, want removed=true repo_id=%q", m, newID)
			}
			// AFTER: the repo is gone but both fixtures remain.
			after := repoIDSet()
			if after[newID] {
				t.Errorf("after remove: eng_list_repos still has %q", newID)
			}
			if !after[coverage.AlphaRepoID] || !after[coverage.BetaRepoID] {
				t.Errorf("after remove: set %v missing a seeded fixture", after)
			}
		}},
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
		{family: f, tool: "eng_set_repo_alias", bead: "solov2-awb9", run: func(t *testing.T) {
			h := newHarness(t)
			aliasesOf := func(repoID string) []string {
				res, rpcErr := h.Call("eng_list_repos", map[string]any{})
				if rpcErr != nil {
					t.Fatalf("eng_list_repos: %v", rpcErr)
				}
				for _, v := range res.(map[string]any)["repos"].([]mcp.RepoView) {
					if v.RepoID == repoID {
						return v.Aliases
					}
				}
				t.Fatalf("eng_list_repos: repo %q not present", repoID)
				return nil
			}
			if before := aliasesOf(coverage.AlphaRepoID); slices.Contains(before, "myalias") || !slices.Contains(before, "alpha") {
				t.Fatalf("before: aliases = %v, want \"alpha\" present and \"myalias\" absent", before)
			}
			res, rpcErr := h.Call("eng_set_repo_alias", map[string]any{"name": "myalias", "repo_id": coverage.AlphaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_set_repo_alias: %v", rpcErr)
			}
			m := res.(map[string]any)
			if m["repo_id"] != coverage.AlphaRepoID || m["name"] != "myalias" {
				t.Errorf("set returned %v, want repo_id=%q name=%q", m, coverage.AlphaRepoID, "myalias")
			}
			if after := aliasesOf(coverage.AlphaRepoID); !slices.Contains(after, "myalias") {
				t.Errorf("after: aliases = %v, want \"myalias\" present", after)
			}
		}},
		{family: f, tool: "eng_remove_repo_alias", bead: "solov2-ffvx", run: func(t *testing.T) {
			h := newHarness(t)
			aliasesOf := func(repoID string) []string {
				res, rpcErr := h.Call("eng_list_repos", map[string]any{})
				if rpcErr != nil {
					t.Fatalf("eng_list_repos: %v", rpcErr)
				}
				for _, v := range res.(map[string]any)["repos"].([]mcp.RepoView) {
					if v.RepoID == repoID {
						return v.Aliases
					}
				}
				t.Fatalf("eng_list_repos: repo %q not present", repoID)
				return nil
			}
			if before := aliasesOf(coverage.AlphaRepoID); !slices.Contains(before, "alpha") {
				t.Fatalf("before: aliases = %v, want \"alpha\" present", before)
			}
			res, rpcErr := h.Call("eng_remove_repo_alias", map[string]any{"name": "alpha"})
			if rpcErr != nil {
				t.Fatalf("eng_remove_repo_alias: %v", rpcErr)
			}
			m := res.(map[string]any)
			if m["removed"] != true || m["name"] != "alpha" {
				t.Errorf("remove returned %v, want removed=true name=%q", m, "alpha")
			}
			if after := aliasesOf(coverage.AlphaRepoID); slices.Contains(after, "alpha") {
				t.Errorf("after: aliases = %v, want \"alpha\" absent", after)
			}
			_, rpcErr = h.Call("eng_remove_repo_alias", map[string]any{"name": "nonexistent-alias"})
			if rpcErr == nil || rpcErr.Code != mcp.CodeNotFound {
				t.Errorf("remove unknown: rpcErr = %v, want CodeNotFound", rpcErr)
			}
		}},
		{family: f, tool: "eng_find_owner", bead: "solov2-a6ud", run: func(t *testing.T) {
			h := newHarness(t)
			// CODEOWNERS path is deterministic (no git blame). The modalpha
			// fixture carries a root CODEOWNERS with `*.go @alpha-team`; the
			// handler resolves Alpha's root_path from the repos table and reads
			// it. file_path is repo-relative — matchesCodeownersPattern falls
			// back to the filename component (series.go) for the unanchored glob.
			res, rpcErr := h.Call("eng_find_owner", map[string]any{
				"file_path": "metric/series.go", "repo_id": coverage.AlphaRepoID,
			})
			if rpcErr != nil {
				t.Fatalf("eng_find_owner: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_find_owner: result type %T, want map[string]any", res)
			}
			if m["source"] != "codeowners" {
				t.Errorf("source = %v, want \"codeowners\"", m["source"])
			}
			if m["owner"] != "@alpha-team" {
				t.Errorf("owner = %v, want \"@alpha-team\"", m["owner"])
			}
		}},
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
		{family: f, tool: "eng_close_finding", bead: "solov2-tvid", run: func(t *testing.T) {
			h := newHarness(t)
			// findingRow is unexported; the handler returns
			// map[string]any{"finding": findingRow}, so read State via JSON.
			getState := func() string {
				res, rpcErr := h.Call("eng_get_finding", map[string]any{"finding_id": "seed-finding-0"})
				if rpcErr != nil {
					t.Fatalf("eng_get_finding: %v", rpcErr)
				}
				var got struct {
					Finding struct {
						State string `json:"state"`
					} `json:"finding"`
				}
				b, _ := json.Marshal(res)
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatalf("decode finding: %v", err)
				}
				return got.Finding.State
			}

			// BEFORE: seed-finding-0 is open.
			if st := getState(); st != "open" {
				t.Fatalf("before close: state = %q, want open", st)
			}

			// MUTATE: warn severity + AGENT actor closes without the human gate.
			res, rpcErr := h.Call("eng_close_finding", map[string]any{
				"finding_id": "seed-finding-0", "reason": "verified safe",
			})
			if rpcErr != nil {
				t.Fatalf("eng_close_finding: %v", rpcErr)
			}
			if m, ok := res.(map[string]any); !ok || m["state"].(string) != "closed" {
				t.Fatalf("close result = %v, want state=closed", res)
			}

			// AFTER: the transition persisted.
			if st := getState(); st != "closed" {
				t.Fatalf("after close: state = %q, want closed", st)
			}

			// Unknown finding_id -> CodeNotFound.
			_, nfErr := h.Call("eng_close_finding", map[string]any{"finding_id": "nope", "reason": "x"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown finding_id: got %v, want CodeNotFound", nfErr)
			}
		}},
		{family: f, tool: "eng_reopen_finding", bead: "solov2-ifne", run: func(t *testing.T) {
			h := newHarness(t)
			// findingRow is unexported; eng_get_finding returns
			// map[string]any{"finding": findingRow}, so read State via JSON.
			getState := func() string {
				res, rpcErr := h.Call("eng_get_finding", map[string]any{"finding_id": "seed-finding-1"})
				if rpcErr != nil {
					t.Fatalf("eng_get_finding: %v", rpcErr)
				}
				var got struct {
					Finding struct {
						State string `json:"state"`
					} `json:"finding"`
				}
				b, _ := json.Marshal(res)
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatalf("decode finding: %v", err)
				}
				return got.Finding.State
			}

			// BEFORE: seed-finding-1 is pre-seeded closed.
			if st := getState(); st != "closed" {
				t.Fatalf("before reopen: state = %q, want closed", st)
			}

			// MUTATE: reopen has no human gate; returns {state: "open"}.
			res, rpcErr := h.Call("eng_reopen_finding", map[string]any{
				"finding_id": "seed-finding-1",
			})
			if rpcErr != nil {
				t.Fatalf("eng_reopen_finding: %v", rpcErr)
			}
			if m, ok := res.(map[string]any); !ok || m["state"].(string) != "open" {
				t.Fatalf("reopen result = %v, want state=open", res)
			}

			// AFTER: the transition persisted.
			if st := getState(); st != "open" {
				t.Fatalf("after reopen: state = %q, want open", st)
			}

			// Unknown finding_id -> CodeNotFound.
			_, nfErr := h.Call("eng_reopen_finding", map[string]any{"finding_id": "nope"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown finding_id: got %v, want CodeNotFound", nfErr)
			}
		}},
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
		{family: f, tool: "eng_suppress_finding", bead: "solov2-uq5t", run: func(t *testing.T) {
			h := newHarness(t)
			// findingRow is unexported in package mcp; round-trip the result
			// through JSON and collect the finding_id set for one rule.
			listFindingIDs := func(repoID, rule string, includeSuppressed bool) map[string]bool {
				res, rpcErr := h.Call("eng_list_findings", map[string]any{
					"repo_id": repoID, "rule": rule, "include_suppressed": includeSuppressed,
				})
				if rpcErr != nil {
					t.Fatalf("eng_list_findings: %v", rpcErr)
				}
				var out struct {
					Findings []struct {
						FindingID string `json:"finding_id"`
					} `json:"findings"`
				}
				b, _ := json.Marshal(res)
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatalf("decode findings: %v", err)
				}
				ids := map[string]bool{}
				for _, fr := range out.Findings {
					ids[fr.FindingID] = true
				}
				return ids
			}

			// BEFORE: the seeded complexity finding is in the default list.
			if !listFindingIDs(coverage.AlphaRepoID, "complexity", false)["seed-finding-0"] {
				t.Fatalf("before suppress: default list missing seed-finding-0")
			}

			// MUTATE: scope defaults to "finding"; branch/repo_id derive from the row.
			res, rpcErr := h.Call("eng_suppress_finding", map[string]any{
				"finding_id": "seed-finding-0", "reason": "accepted complexity",
			})
			if rpcErr != nil {
				t.Fatalf("eng_suppress_finding: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("eng_suppress_finding: result type %T, want map[string]any", res)
			}
			if m["scope"] != "finding" {
				t.Errorf("scope = %v, want \"finding\"", m["scope"])
			}
			if id, _ := m["suppression_id"].(string); id == "" {
				t.Errorf("suppression_id = %v, want non-empty string", m["suppression_id"])
			}

			// AFTER: dropped from the default list, but still present (suppressed,
			// not deleted) when include_suppressed=true.
			if listFindingIDs(coverage.AlphaRepoID, "complexity", false)["seed-finding-0"] {
				t.Errorf("after suppress: default list still contains seed-finding-0")
			}
			if !listFindingIDs(coverage.AlphaRepoID, "complexity", true)["seed-finding-0"] {
				t.Errorf("after suppress: include_suppressed list missing seed-finding-0")
			}
		}},
		{family: f, tool: "eng_get_suppression", bead: "solov2-9735", run: func(t *testing.T) {
			h := newHarness(t)
			// suppressionRow is unexported in package mcp and the handler returns
			// map[string]any{"suppression": suppressionRow}; round-trip via JSON.
			type srow struct {
				SuppressionID string `json:"suppression_id"`
				Scope         string `json:"scope"`
				Target        string `json:"target"`
				Rule          string `json:"rule"`
				Reason        string `json:"reason"`
			}
			res, rpcErr := h.Call("eng_get_suppression", map[string]any{"suppression_id": "seed-suppression-0"})
			if rpcErr != nil {
				t.Fatalf("eng_get_suppression: %v", rpcErr)
			}
			var got struct {
				Suppression srow `json:"suppression"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode suppression: %v", err)
			}
			sr := got.Suppression
			wantTarget := string(h.ResolveID(coverage.AlphaRepoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "ComputeVariance"}))
			if sr.SuppressionID != "seed-suppression-0" || sr.Scope != "node" || sr.Rule != "complexity" || sr.Target != wantTarget {
				t.Errorf("seed-suppression-0 = %+v, want id=seed-suppression-0 scope=node rule=complexity target=%q", sr, wantTarget)
			}
			if !strings.Contains(sr.Reason, "intentionally explicit") {
				t.Errorf("seed-suppression-0 reason %q missing %q", sr.Reason, "intentionally explicit")
			}
			// Unknown suppression_id resolves to zero rows -> CodeNotFound.
			_, nfErr := h.Call("eng_get_suppression", map[string]any{"suppression_id": "seed-suppression-nope"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown suppression_id: got %v, want CodeNotFound", nfErr)
			}
		}},
		{family: f, tool: "eng_list_suppressions", bead: "solov2-avb5", run: func(t *testing.T) {
			h := newHarness(t)
			// suppressionRow is unexported in package mcp; round-trip the result
			// through JSON into a local shape rather than type-asserting it.
			type srow struct {
				SuppressionID string `json:"suppression_id"`
				Scope         string `json:"scope"`
				Rule          string `json:"rule"`
				Reason        string `json:"reason"`
				ActorKind     string `json:"actor_kind"`
			}
			res, rpcErr := h.Call("eng_list_suppressions", map[string]any{
				"repo_id": coverage.AlphaRepoID, "branch": coverage.FixtureBranch,
			})
			if rpcErr != nil {
				t.Fatalf("eng_list_suppressions: %v", rpcErr)
			}
			var out struct {
				Suppressions []srow `json:"suppressions"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("decode suppressions: %v", err)
			}
			bySeed := map[string]srow{}
			for _, s := range out.Suppressions {
				bySeed[s.SuppressionID] = s
			}
			// Contains-all (not exact size): assert the seeded suppression is present.
			sr, ok := bySeed["seed-suppression-0"]
			if !ok {
				t.Fatalf("suppression set %v missing seed-suppression-0", bySeed)
			}
			if sr.Scope != "node" || sr.Rule != "complexity" || sr.ActorKind != "agent" {
				t.Errorf("seed-suppression-0 = %+v, want scope=node rule=complexity actor_kind=agent", sr)
			}
			if !strings.Contains(sr.Reason, "intentionally explicit") {
				t.Errorf("seed-suppression-0 reason %q missing %q", sr.Reason, "intentionally explicit")
			}
		}},
		{family: f, tool: "eng_close_suppression", bead: "solov2-lmhn", run: func(t *testing.T) {
			h := newHarness(t)
			// suppressionRow is unexported; eng_get_suppression returns
			// map[string]any{"suppression": suppressionRow}. expires_at is *int64
			// omitempty: ABSENT in JSON while active, PRESENT after close.
			getExpiresAt := func() *int64 {
				res, rpcErr := h.Call("eng_get_suppression", map[string]any{"suppression_id": "seed-suppression-0"})
				if rpcErr != nil {
					t.Fatalf("eng_get_suppression: %v", rpcErr)
				}
				var got struct {
					Suppression struct {
						ExpiresAt *int64 `json:"expires_at"`
					} `json:"suppression"`
				}
				b, _ := json.Marshal(res)
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatalf("decode suppression: %v", err)
				}
				return got.Suppression.ExpiresAt
			}

			// BEFORE: seed-suppression-0 is active (expires_at unset).
			if exp := getExpiresAt(); exp != nil {
				t.Fatalf("before close: expires_at = %d, want nil (active)", *exp)
			}

			// MUTATE: close sets expires_at = now; returned map value is a native int64.
			res, rpcErr := h.Call("eng_close_suppression", map[string]any{"suppression_id": "seed-suppression-0"})
			if rpcErr != nil {
				t.Fatalf("eng_close_suppression: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("close result type = %T, want map", res)
			}
			if exp, ok := m["expires_at"].(int64); !ok || exp <= 0 {
				t.Fatalf("close expires_at = %v (%T), want positive int64", m["expires_at"], m["expires_at"])
			}

			// AFTER: the suppression is terminated (expires_at now set).
			if exp := getExpiresAt(); exp == nil || *exp <= 0 {
				t.Fatalf("after close: expires_at = %v, want positive (terminated)", exp)
			}

			// Unknown suppression_id -> CodeNotFound.
			_, nfErr := h.Call("eng_close_suppression", map[string]any{"suppression_id": "nope"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown suppression_id: got %v, want CodeNotFound", nfErr)
			}
		}},
	}
}

// taskFamily holds the 3 PARKED task tools — covered only via the WithTaskTools
// opt-in (production registers 37, not 40). task=true marks the opt-in need.
func taskFamily() []coverageTool {
	const f = "task"
	return []coverageTool{
		{family: f, tool: "eng_set_active_task", bead: "solov2-orrj", task: true, run: func(t *testing.T) {
			h := newHarness(t, WithTaskTools())
			if got := activeTaskID(t, h); got != "task-active" {
				t.Fatalf("before: active task = %q, want task-active", got)
			}
			res, rpcErr := h.Call("eng_set_active_task", map[string]any{"task_id": "task-done", "repo_id": coverage.BetaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_set_active_task: %v", rpcErr)
			}
			m, ok := res.(map[string]any)
			if !ok || m["task_id"] != "task-done" {
				t.Fatalf("set result = %v (%T), want map task_id=task-done", res, res)
			}
			if got := activeTaskID(t, h); got != "task-done" {
				t.Errorf("after: active task = %q, want task-done", got)
			}
			_, nfErr := h.Call("eng_set_active_task", map[string]any{"task_id": "task-nonexistent", "repo_id": coverage.BetaRepoID})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Errorf("set unknown: rpcErr = %v, want CodeNotFound", nfErr)
			}
		}},
		{family: f, tool: "eng_get_active_task", bead: "solov2-0cgj", task: true, run: func(t *testing.T) {
			h := newHarness(t, WithTaskTools())
			// taskRow is unexported in package mcp; the handler returns it
			// directly when a task is active, so round-trip via JSON.
			res, rpcErr := h.Call("eng_get_active_task", map[string]any{"repo_id": coverage.BetaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_get_active_task: %v", rpcErr)
			}
			var got struct {
				TaskID string `json:"task_id"`
				Title  string `json:"title"`
				Active int    `json:"active"`
				RepoID string `json:"repo_id"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode active task: %v", err)
			}
			if got.TaskID != "task-active" || got.Title != "wire up the badge widget" || got.Active != 1 || got.RepoID != coverage.BetaRepoID {
				t.Errorf("active task = %+v, want task_id=task-active title=%q active=1 repo_id=%s", got, "wire up the badge widget", coverage.BetaRepoID)
			}
			// Alpha has no tasks -> null shape map[string]any{"task_id": nil}.
			none, noneErr := h.Call("eng_get_active_task", map[string]any{"repo_id": coverage.AlphaRepoID})
			if noneErr != nil {
				t.Fatalf("eng_get_active_task (no active): %v", noneErr)
			}
			m, ok := none.(map[string]any)
			if !ok {
				t.Fatalf("no-active result type %T, want map[string]any", none)
			}
			if m["task_id"] != nil {
				t.Errorf("no-active task_id = %v, want nil", m["task_id"])
			}
		}},
		{family: f, tool: "eng_get_task_history", bead: "solov2-58sw", task: true, run: func(t *testing.T) {
			h := newHarness(t, WithTaskTools())
			res, rpcErr := h.Call("eng_get_task_history", map[string]any{"repo_id": coverage.BetaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_get_task_history: %v", rpcErr)
			}
			// taskRow is unexported in package mcp; round-trip via JSON.
			var got struct {
				Tasks []struct {
					TaskID string `json:"task_id"`
					Title  string `json:"title"`
				} `json:"tasks"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode task history: %v", err)
			}
			// Same created_at on both seeds -> assert as a SET, not an order.
			titles := map[string]string{}
			for _, tk := range got.Tasks {
				titles[tk.TaskID] = tk.Title
			}
			if titles["task-active"] != "wire up the badge widget" {
				t.Errorf("task-active title = %q, want %q", titles["task-active"], "wire up the badge widget")
			}
			if titles["task-done"] != "scaffold the beta module" {
				t.Errorf("task-done title = %q, want %q", titles["task-done"], "scaffold the beta module")
			}
			// Alpha has no tasks.
			none, noneErr := h.Call("eng_get_task_history", map[string]any{"repo_id": coverage.AlphaRepoID})
			if noneErr != nil {
				t.Fatalf("eng_get_task_history (alpha): %v", noneErr)
			}
			var alpha struct {
				Tasks []json.RawMessage `json:"tasks"`
			}
			ab, _ := json.Marshal(none)
			if err := json.Unmarshal(ab, &alpha); err != nil {
				t.Fatalf("decode alpha history: %v", err)
			}
			if len(alpha.Tasks) != 0 {
				t.Errorf("alpha task count = %d, want 0", len(alpha.Tasks))
			}
		}},
	}
}

// activeTaskID returns the Beta repo's active task id via eng_get_active_task.
// taskRow is unexported in package mcp, so round-trip the result through JSON.
func activeTaskID(t *testing.T, h *toolHarness) string {
	t.Helper()
	res, rpcErr := h.Call("eng_get_active_task", map[string]any{"repo_id": coverage.BetaRepoID})
	if rpcErr != nil {
		t.Fatalf("eng_get_active_task: %v", rpcErr)
	}
	var got struct {
		TaskID string `json:"task_id"`
	}
	b, _ := json.Marshal(res)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode active task: %v", err)
	}
	return got.TaskID
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
		{family: f, tool: "eng_find_related", bead: "solov2-d217", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			// Anchor at line 39 inside computeMean's body (metric/series.go). The
			// handler resolves the SMALLEST ENCLOSING node (computeMean) and reuses
			// the eng_search_similar vector core, so the same near-dup partner
			// (averageSamples) surfaces. file_path is matched verbatim against the
			// stored ABSOLUTE node paths — pass an absolute path. Assert ranking
			// INVARIANTS only: seed-exclusion, descending ORDERING, near-dup
			// MEMBERSHIP by node_id — never absolute vector scores.
			seed := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "computeMean"}))
			want := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/deviation.go", Kind: domain.KindFunction, Name: "averageSamples"}))
			res, rpcErr := h.Call("eng_find_related", map[string]any{
				"file_path": filepath.Join(h.Root(repoID), "metric/series.go"),
				"line":      39, "repo_id": repoID, "k": 10,
			})
			if rpcErr != nil {
				t.Fatalf("eng_find_related: %v", rpcErr)
			}
			resp, ok := res.(mcp.SearchResponse)
			if !ok {
				t.Fatalf("eng_find_related: result type %T, want mcp.SearchResponse", res)
			}
			if len(resp.Results) == 0 {
				t.Fatal("eng_find_related returned no neighbours for the enclosing seed")
			}
			found := false
			for i, hit := range resp.Results {
				if hit.NodeID == seed {
					t.Errorf("enclosing seed %q must be excluded from its own neighbours", seed)
				}
				if hit.NodeID == want {
					found = true
				}
				// ORDERING: scores are non-increasing.
				if i > 0 && hit.Score > resp.Results[i-1].Score {
					t.Errorf("neighbours not sorted descending: hit[%d] %v > hit[%d] %v",
						i, hit.Score, i-1, resp.Results[i-1].Score)
				}
			}
			if !found {
				t.Errorf("near-dup averageSamples %q missing from computeMean neighbours", want)
			}
		}},
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
		{family: f, tool: "eng_get_dirty_blast_radius", bead: "solov2-1sya", run: func(t *testing.T) {
			// Tier-3: a real git repo, indexed clean, then a watcher-staged
			// uncommitted edit. c1 commits Helper (callee) + Caller (CALLS Helper).
			// stageDirtyEdit simulates the fsnotify hot path writing an edited
			// Helper into the SHARED staging.Area the dirty-blast service reads, so
			// the callers blast must surface Caller, the affected caller.
			root := initGitRepo(t)
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 1 }\n")
			writeRepoFile(t, root, "caller.go", "package p\n\nfunc Caller() int { return Helper() }\n")
			gitCommitAll(t, root, "c1")

			h := newHarness(t)
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": root})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			repoID, _ := res.(map[string]any)["repo_id"].(string)
			if repoID == "" {
				t.Fatalf("eng_add_repo returned empty repo_id (got %v)", res)
			}
			// Cold-scan the committed tree so Helper, Caller and the CALLS edge land
			// in the graph; promotion drains the shared staging area back to clean.
			if _, e := h.Call("eng_reindex_repo", map[string]any{"repo_id": repoID}); e != nil {
				t.Fatalf("eng_reindex_repo: %v", e)
			}
			// Simulate an uncommitted working-tree edit to helper.go via the same
			// Save the watcher uses, staging it under (repoID, FixtureBranch).
			h.stageDirtyEdit(repoID, coverage.FixtureBranch,
				filepath.Join(root, "helper.go"),
				[]byte("package p\n\nfunc Helper() int { return 2 }\n"))

			res, rpcErr = h.Call("eng_get_dirty_blast_radius", map[string]any{
				"repo_id": repoID, "direction": string(blastradius.DirCallers), "max_depth": 5,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_dirty_blast_radius: %v", rpcErr)
			}
			resp, ok := res.(mcp.BlastResponse)
			if !ok {
				t.Fatalf("eng_get_dirty_blast_radius: result type %T, want mcp.BlastResponse", res)
			}
			// The dirty Helper is the seed and may ride along; assert CONTAINMENT
			// of "Caller", its affected caller. Match by Name (different repo than
			// the coverage manifest).
			names := make([]string, 0, len(resp.Entries))
			found := false
			for _, e := range resp.Entries {
				names = append(names, e.Name)
				if e.Name == "Caller" {
					found = true
				}
			}
			t.Logf("dirty-blast entries: %v", names)
			if !found {
				t.Errorf("affected caller %q missing from dirty blast radius (got %v)", "Caller", names)
			}
		}},
		{family: f, tool: "eng_get_diff_blast_radius", bead: "solov2-56c8", run: func(t *testing.T) {
			// Tier-3: a real git repo that is ALSO indexed. c1 seeds Helper
			// (callee) + Caller (caller, CALLS Helper). c2 edits ONLY helper.go.
			// Diffing HEAD~1..HEAD changes {helper.go} → its node Helper → the
			// callers blast must surface Caller, the affected caller.
			root := initGitRepo(t)
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 1 }\n")
			writeRepoFile(t, root, "caller.go", "package p\n\nfunc Caller() int { return Helper() }\n")
			gitCommitAll(t, root, "c1")
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 2 }\n")
			gitCommitAll(t, root, "c2")

			h := newHarness(t)
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": root})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			repoID, _ := res.(map[string]any)["repo_id"].(string)
			if repoID == "" {
				t.Fatalf("eng_add_repo returned empty repo_id (got %v)", res)
			}
			// Cold-scan the working tree so Helper, Caller and the CALLS edge
			// land in the graph the blast tools read.
			if _, e := h.Call("eng_reindex_repo", map[string]any{"repo_id": repoID}); e != nil {
				t.Fatalf("eng_reindex_repo: %v", e)
			}

			res, rpcErr = h.Call("eng_get_diff_blast_radius", map[string]any{
				"repo_id": repoID, "ref_a": "HEAD~1", "ref_b": "HEAD",
				"direction": string(blastradius.DirCallers), "max_depth": 5,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_diff_blast_radius: %v", rpcErr)
			}
			resp, ok := res.(mcp.BlastResponse)
			if !ok {
				t.Fatalf("eng_get_diff_blast_radius: result type %T, want mcp.BlastResponse", res)
			}
			// The changed Helper is the seed and may ride along (depth 0); assert
			// CONTAINMENT of "Caller" (its affected caller). Different repo than
			// the coverage manifest, so match by Name (entry embeds nodeDTO).
			names := make([]string, 0, len(resp.Entries))
			found := false
			for _, e := range resp.Entries {
				names = append(names, e.Name)
				if e.Name == "Caller" {
					found = true
				}
			}
			t.Logf("diff-blast entries: %v", names)
			if !found {
				t.Errorf("affected caller %q missing from diff blast radius (got %v)", "Caller", names)
			}
		}},
	}
}

func searchFamily() []coverageTool {
	const f = "search"
	return []coverageTool{
		{family: f, tool: "eng_search_semantic", bead: "solov2-4g0h", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			// Identifier query: lexical (FTS) fusion deterministically surfaces the
			// ComputeVariance node regardless of static-embedder recall, so assert
			// MEMBERSHIP (by node_id) and descending ORDERING — never absolute RRF
			// scores, which cluster in ~0.016–0.033 and are query-relative only.
			want := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "ComputeVariance"}))
			res, rpcErr := h.Call("eng_search_semantic", map[string]any{
				"query": "ComputeVariance", "repo_id": repoID, "k": 10,
			})
			if rpcErr != nil {
				t.Fatalf("eng_search_semantic: %v", rpcErr)
			}
			resp, ok := res.(mcp.SearchResponse)
			if !ok {
				t.Fatalf("eng_search_semantic: result type %T, want mcp.SearchResponse", res)
			}
			if len(resp.Results) == 0 {
				t.Fatal("eng_search_semantic returned no results for an indexed identifier")
			}
			// MEMBERSHIP: the queried symbol's node is present (containment, not
			// position) — a function-name query may also surface sibling/chunk nodes.
			found := false
			for i, hit := range resp.Results {
				if hit.NodeID == want {
					found = true
				}
				// ORDERING: scores are non-increasing (sorted descending).
				if i > 0 && hit.Score > resp.Results[i-1].Score {
					t.Errorf("results not sorted descending: hit[%d] score %v > hit[%d] score %v",
						i, hit.Score, i-1, resp.Results[i-1].Score)
				}
			}
			if !found {
				t.Errorf("ComputeVariance node %q missing from eng_search_semantic results", want)
			}
		}},
		{family: f, tool: "eng_search_similar", bead: "solov2-r1ue", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			// Seed: computeMean. averageSamples is its frozenClones near-dup
			// partner (facts.go), so it surfaces among the static-embedder
			// neighbours. Assert ranking INVARIANTS only — never absolute
			// vector scores: seed-exclusion, descending ORDERING, and
			// near-dup MEMBERSHIP by node_id.
			seed := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "computeMean"}))
			want := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/deviation.go", Kind: domain.KindFunction, Name: "averageSamples"}))
			res, rpcErr := h.Call("eng_search_similar", map[string]any{
				"node_id": seed, "repo_id": repoID, "k": 10,
			})
			if rpcErr != nil {
				t.Fatalf("eng_search_similar: %v", rpcErr)
			}
			resp, ok := res.(mcp.SearchResponse)
			if !ok {
				t.Fatalf("eng_search_similar: result type %T, want mcp.SearchResponse", res)
			}
			if len(resp.Results) == 0 {
				t.Fatal("eng_search_similar returned no neighbours for an embedded seed")
			}
			found := false
			for i, hit := range resp.Results {
				if hit.NodeID == seed {
					t.Errorf("seed node %q must be excluded from its own neighbours", seed)
				}
				if hit.NodeID == want {
					found = true
				}
				// ORDERING: scores are non-increasing.
				if i > 0 && hit.Score > resp.Results[i-1].Score {
					t.Errorf("neighbours not sorted descending: hit[%d] %v > hit[%d] %v",
						i, hit.Score, i-1, resp.Results[i-1].Score)
				}
			}
			if !found {
				t.Errorf("near-dup averageSamples %q missing from computeMean neighbours", want)
			}
		}},
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
		{family: f, tool: "eng_get_hot_zone", bead: "solov2-17kd", run: func(t *testing.T) {
			// Tier-3: a real git repo with MULTIPLE RECENT commits, indexed.
			// hot_zone ranks files by change-risk = recent-change-frequency ×
			// blast-radius. helper.go is churned 3× inside the 30-day window
			// (high frequency) and Caller depends on it (blast-radius > 0 via
			// the default callers direction), so it must score above caller.go.
			// Commits use gitCommitAllNow so their dates fall inside the
			// look-back window (gitCommitAll pins 2025-01-01, which is not).
			root := initGitRepo(t)
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 1 }\n")
			writeRepoFile(t, root, "caller.go", "package p\n\nfunc Caller() int { return Helper() }\n")
			gitCommitAllNow(t, root, "c1")
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 2 }\n")
			gitCommitAllNow(t, root, "c2")
			writeRepoFile(t, root, "helper.go", "package p\n\nfunc Helper() int { return 3 }\n")
			gitCommitAllNow(t, root, "c3")

			h := newHarness(t)
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": root})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			repoID, _ := res.(map[string]any)["repo_id"].(string)
			if repoID == "" {
				t.Fatalf("eng_add_repo returned empty repo_id (got %v)", res)
			}
			// Cold-scan so Helper, Caller and the CALLS edge land in the graph,
			// giving Helper a non-zero blast radius (Caller depends on it).
			if _, e := h.Call("eng_reindex_repo", map[string]any{"repo_id": repoID}); e != nil {
				t.Fatalf("eng_reindex_repo: %v", e)
			}

			res, rpcErr = h.Call("eng_get_hot_zone", map[string]any{"repo_id": repoID})
			if rpcErr != nil {
				t.Fatalf("eng_get_hot_zone: %v", rpcErr)
			}
			resp, ok := res.(mcp.HotZoneResponse)
			if !ok {
				t.Fatalf("eng_get_hot_zone: result type %T, want mcp.HotZoneResponse", res)
			}
			var helper *wiki.HotZone
			for i := range resp.Zones {
				z := &resp.Zones[i]
				t.Logf("zone: file=%s freq=%d blast=%d score=%d",
					z.FilePath, z.RecentChangeFrequency, z.BlastRadius, z.Score)
				if strings.HasSuffix(z.FilePath, "helper.go") {
					helper = z
				}
			}
			if helper == nil {
				t.Fatalf("helper.go not ranked (degraded=%v hint=%q zones=%+v)",
					resp.DegradedReasons, resp.Hint, resp.Zones)
			}
			if helper.RecentChangeFrequency < 2 {
				t.Errorf("helper.go RecentChangeFrequency=%d, want >= 2", helper.RecentChangeFrequency)
			}
			if helper.BlastRadius < 1 {
				t.Errorf("helper.go BlastRadius=%d, want >= 1", helper.BlastRadius)
			}
			if helper.Score <= 0 {
				t.Errorf("helper.go Score=%d, want > 0", helper.Score)
			}
		}},
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
		{family: f, tool: "eng_get_context_pack", bead: "solov2-xjjk", run: func(t *testing.T) {
			h := newHarness(t)
			repoID := coverage.AlphaRepoID
			seed := string(h.ResolveID(repoID, coverage.NodeKey{
				Path: "metric/series.go", Kind: domain.KindFunction, Name: "ComputeVariance"}))
			res, rpcErr := h.Call("eng_get_context_pack", map[string]any{
				"node_id": seed, "repo_id": repoID,
			})
			if rpcErr != nil {
				t.Fatalf("eng_get_context_pack: %v", rpcErr)
			}
			// contextPackResponse is unexported in package mcp and embeds the
			// application Pack anonymously, so Pack's fields are JSON-promoted to
			// the top level — decode them there, not under a "pack" key.
			var resp struct {
				Nodes []struct {
					NodeID  string `json:"node_id"`
					HasOpen bool   `json:"has_open_finding"`
				} `json:"nodes"`
				OpenFindings []struct {
					NodeID string `json:"node_id"`
				} `json:"open_findings"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &resp); err != nil {
				t.Fatalf("decode context pack: %v", err)
			}
			// The pack anchors on ComputeVariance; its node must be present (with
			// blast neighbours) and flagged as carrying the open finding. Assert
			// CONTAINMENT — the node set also holds blast-radius neighbours.
			var anchor struct {
				present bool
				hasOpen bool
			}
			for _, n := range resp.Nodes {
				if n.NodeID == seed {
					anchor.present, anchor.hasOpen = true, n.HasOpen
				}
			}
			if !anchor.present {
				t.Fatalf("ComputeVariance (%s) missing from pack nodes %v", seed, resp.Nodes)
			}
			if !anchor.hasOpen {
				t.Errorf("ComputeVariance node has_open_finding = false, want true")
			}
			// seed-finding-0 is the seeded OPEN complexity finding anchored on
			// ComputeVariance; OpenFindings carries node IDs, so the anchor's id
			// must appear there.
			openByNode := map[string]bool{}
			for _, fnd := range resp.OpenFindings {
				openByNode[fnd.NodeID] = true
			}
			if !openByNode[seed] {
				t.Errorf("OpenFindings %v missing ComputeVariance node %s (seed-finding-0)", resp.OpenFindings, seed)
			}
		}},
	}
}

func dependencyFamily() []coverageTool {
	const f = "dependency"
	return []coverageTool{
		{family: f, tool: "eng_list_dependencies", bead: "solov2-1zhc", run: func(t *testing.T) {
			h := newHarness(t)
			// Beta CALLS into modalpha/metric (the one genuine cross-module dep);
			// modalpha imports nothing external. Assert CONTAINS, not exact set:
			// solov2-tb74 made syncFileImports subtract modbeta's OWN module path,
			// so "example.com/modbeta/widget" no longer leaks in; contains-all
			// stays valid regardless.
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
		{family: f, tool: "eng_find_clones", bead: "solov2-8jfs", run: func(t *testing.T) {
			h := newHarness(t)
			// Structural-invariants-only coverage (fuzzy DoD). The manifest's
			// computeMean/averageSamples are a *near*-dup pair, not byte-identical,
			// so membership is NOT assertable here: near mode is empty (this harness
			// runs no autolink → no scored SIMILAR_TO edges), and exact mode in this
			// fixture buckets every non-excluded symbol under an empty content_hash
			// (likely a content_hash-not-populated defect on this index path), which
			// is a defect artifact we must not freeze. So assert only the invariants
			// robust to a content_hash fix (0 groups must pass) plus exact's real
			// kind-exclusion contract.
			excluded := map[string]bool{
				string(domain.KindPackage): true, string(domain.KindChunk): true,
				string(domain.KindFile): true, string(domain.KindModule): true,
				string(domain.KindField): true, "import": true,
			}
			ex, exErr := h.Call("eng_find_clones", map[string]any{"repo_id": coverage.AlphaRepoID, "mode": "exact"})
			if exErr != nil {
				t.Fatalf("eng_find_clones exact: %v", exErr)
			}
			exResp, ok := ex.(mcp.FindClonesResponse)
			if !ok {
				t.Fatalf("exact: result type %T, want mcp.FindClonesResponse", ex)
			}
			if exResp.Mode != "exact" {
				t.Errorf("exact: mode = %q, want \"exact\"", exResp.Mode)
			}
			if exResp.Groups == nil {
				t.Error("exact: groups is nil, want non-nil")
			}
			for _, g := range exResp.Groups {
				if g.Size != len(g.Members) || g.Size < 2 {
					t.Errorf("exact group: size=%d members=%d, want size==members>=2", g.Size, len(g.Members))
				}
				for _, m := range g.Members {
					if excluded[m.Kind] {
						t.Errorf("exact group member %q has excluded kind %q", m.Name, m.Kind)
					}
				}
			}
			nr, nrErr := h.Call("eng_find_clones", map[string]any{"repo_id": coverage.AlphaRepoID, "mode": "near", "min_score": 0.0})
			if nrErr != nil {
				t.Fatalf("eng_find_clones near: %v", nrErr)
			}
			nrResp, ok := nr.(mcp.FindClonesResponse)
			if !ok {
				t.Fatalf("near: result type %T, want mcp.FindClonesResponse", nr)
			}
			if nrResp.Mode != "near" {
				t.Errorf("near: mode = %q, want \"near\"", nrResp.Mode)
			}
			if nrResp.Clusters == nil {
				t.Error("near: clusters is nil, want non-nil")
			}
			for _, c := range nrResp.Clusters {
				if c.MinScore > c.MaxScore {
					t.Errorf("near cluster: min_score=%v > max_score=%v", c.MinScore, c.MaxScore)
				}
				if c.Size != len(c.Members) || c.Size < 2 {
					t.Errorf("near cluster: size=%d members=%d, want size==members>=2", c.Size, len(c.Members))
				}
			}
		}},
	}
}

func changedSymbolsFamily() []coverageTool {
	const f = "changed_symbols"
	return []coverageTool{
		{family: f, tool: "eng_find_changed_symbols", bead: "solov2-m2wp", run: func(t *testing.T) {
			// Build a controlled two-commit repo: c1 has Existing only, c2
			// adds Added. Diffing HEAD~1..HEAD must report Added as added and
			// leave Existing (unchanged) out of the added set.
			root := initGitRepo(t)
			writeRepoFile(t, root, "calc.go", "package calc\n\nfunc Existing() int { return 1 }\n")
			gitCommitAll(t, root, "c1")
			writeRepoFile(t, root, "calc.go", "package calc\n\nfunc Existing() int { return 1 }\n\nfunc Added() int { return 2 }\n")
			gitCommitAll(t, root, "c2")

			h := newHarness(t)
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": root})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			repoID, _ := res.(map[string]any)["repo_id"].(string)
			if repoID == "" {
				t.Fatalf("eng_add_repo returned empty repo_id (got %v)", res)
			}

			res, rpcErr = h.Call("eng_find_changed_symbols", map[string]any{
				"repo_id": repoID, "ref_a": "HEAD~1", "ref_b": "HEAD",
			})
			if rpcErr != nil {
				t.Fatalf("eng_find_changed_symbols: %v", rpcErr)
			}
			// Handler returns a changedsymbols.Result value (no serialization
			// through Call), so a direct type-assert is correct.
			r, ok := res.(changedsymbols.Result)
			if !ok {
				t.Fatalf("result type %T, want changedsymbols.Result", res)
			}
			found := false
			for _, sc := range r.Added {
				if sc.Name == "Added" {
					found = true
					if sc.Kind != domain.KindFunction {
						t.Errorf("Added symbol kind = %q, want %q", sc.Kind, domain.KindFunction)
					}
				}
				if sc.Name == "Existing" {
					t.Errorf("unchanged symbol Existing reported as added: %+v", sc)
				}
			}
			if !found {
				t.Errorf("Added set does not contain symbol %q (got %+v)", "Added", r.Added)
			}
		}},
	}
}

func promotionFamily() []coverageTool {
	const f = "promotion"
	return []coverageTool{
		{family: f, tool: "eng_promote_repo", bead: "solov2-0buu", run: func(t *testing.T) {
			// Controlled repo so promote drives a clean 0→N graph transition.
			// ChangedFiles diffs HEAD against its parent, so the symbol must land
			// in a NON-root commit (c1 seeds the file empty; c2 adds Render and is
			// the HEAD whose changed set is {widget.go}).
			root := initGitRepo(t)
			writeRepoFile(t, root, "widget.go", "package widget\n")
			gitCommitAll(t, root, "c1")
			writeRepoFile(t, root, "widget.go", "package widget\n\nfunc Render() string { return \"x\" }\n")
			gitCommitAll(t, root, "c2")

			h := newHarness(t)
			res, rpcErr := h.Call("eng_add_repo", map[string]any{"root_path": root})
			if rpcErr != nil {
				t.Fatalf("eng_add_repo: %v", rpcErr)
			}
			repoID, _ := res.(map[string]any)["repo_id"].(string)
			if repoID == "" {
				t.Fatalf("eng_add_repo returned empty repo_id (got %v)", res)
			}

			fileNodes := func() mcp.GraphResponse {
				r, e := h.Call("eng_get_file_nodes", map[string]any{"file_path": "widget.go", "repo_id": repoID})
				if e != nil {
					t.Fatalf("eng_get_file_nodes: %v", e)
				}
				resp, ok := r.(mcp.GraphResponse)
				if !ok {
					t.Fatalf("eng_get_file_nodes: result type %T, want mcp.GraphResponse", r)
				}
				return resp
			}

			// BEFORE: registered but not yet scanned — zero nodes.
			if n := fileNodes().Nodes; len(n) != 0 {
				t.Fatalf("before promote: got %d nodes, want 0 (unindexed)", len(n))
			}

			// MUTATE: promote re-stages HEAD's files and flushes staging.
			res, rpcErr = h.Call("eng_promote_repo", map[string]any{"repo_id": repoID})
			if rpcErr != nil {
				t.Fatalf("eng_promote_repo: %v", rpcErr)
			}
			// promoteResult is unexported in package mcp; round-trip via JSON.
			var pr struct {
				RepoID        string `json:"repo_id"`
				FilesPromoted int    `json:"files_promoted"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &pr); err != nil {
				t.Fatalf("decode promote result: %v", err)
			}
			if pr.RepoID != repoID || pr.FilesPromoted < 1 {
				t.Fatalf("promote = %+v, want repo_id=%s files_promoted>=1", pr, repoID)
			}

			// AFTER: the graph now holds the file's symbols, including Render.
			after := fileNodes().Nodes
			if len(after) == 0 {
				t.Fatalf("after promote: got 0 nodes, want >0")
			}
			found := false
			for _, n := range after {
				if n.Name == "Render" && n.Kind == string(domain.KindFunction) {
					found = true
				}
			}
			if !found {
				t.Errorf("after promote: no Render (KindFunction) node in %+v", after)
			}
		}},
		{family: f, tool: "eng_reindex_repo", bead: "solov2-extk", run: func(t *testing.T) {
			h := newHarness(t)
			// Count non-zero nodes for a known file; reindex is idempotent, so the
			// count must be stable across a re-run of the cold scan.
			countNodes := func() int {
				res, rpcErr := h.Call("eng_get_file_nodes", map[string]any{
					"file_path": "metric/series.go", "repo_id": coverage.AlphaRepoID,
				})
				if rpcErr != nil {
					t.Fatalf("eng_get_file_nodes: %v", rpcErr)
				}
				resp, ok := res.(mcp.GraphResponse)
				if !ok {
					t.Fatalf("eng_get_file_nodes: result type %T, want mcp.GraphResponse", res)
				}
				return len(resp.Nodes)
			}
			// BEFORE: the file is already indexed (7 non-chunk nodes + chunks).
			n0 := countNodes()
			if n0 == 0 {
				t.Fatal("before reindex: eng_get_file_nodes returned 0 nodes")
			}
			// MUTATE: reindex re-runs the cold scan synchronously. reindexResult is
			// unexported in package mcp, so round-trip the result through JSON.
			res, rpcErr := h.Call("eng_reindex_repo", map[string]any{"repo_id": coverage.AlphaRepoID})
			if rpcErr != nil {
				t.Fatalf("eng_reindex_repo: %v", rpcErr)
			}
			var got struct {
				RepoID string `json:"repo_id"`
				Branch string `json:"branch"`
				Status string `json:"status"`
			}
			b, _ := json.Marshal(res)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("decode reindex result: %v", err)
			}
			if got.Status != "complete" || got.RepoID != coverage.AlphaRepoID || got.Branch != "main" {
				t.Errorf("reindex = %+v, want status=complete repo_id=%s branch=main", got, coverage.AlphaRepoID)
			}
			// AFTER: idempotent reindex left the graph intact and non-zero.
			if n1 := countNodes(); n1 != n0 {
				t.Errorf("after reindex: node count = %d, want %d (idempotent)", n1, n0)
			}
			// Unknown repo_id -> CodeNotFound.
			_, nfErr := h.Call("eng_reindex_repo", map[string]any{"repo_id": "nope"})
			if nfErr == nil || nfErr.Code != mcp.CodeNotFound {
				t.Fatalf("unknown repo_id: got %v, want CodeNotFound", nfErr)
			}
		}},
	}
}

// TestToolCoverage runs the per-family subtest tree. Each tool is a leaf:
// either a real (bead-supplied) assertion or a PENDING skip. The skip MUST name
// the owning bead so `go test -run TestToolCoverage -v` doubles as a checklist.
func TestToolCoverage(t *testing.T) {
	for _, ct := range coverageTools() {
		t.Run(ct.family+"/"+ct.tool, func(t *testing.T) {
			if ct.run == nil {
				t.Skipf("pending: %s — replace this leaf's run func with real assertions", ct.bead)
				return
			}
			ct.run(t)
		})
	}
}
