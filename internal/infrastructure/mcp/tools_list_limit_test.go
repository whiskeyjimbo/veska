// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// makeCloneGroups builds n exact-clone groups for cap testing.
func makeCloneGroups(n int) []duplicates.CloneGroup {
	out := make([]duplicates.CloneGroup, 0, n)
	for range n {
		out = append(out, duplicates.CloneGroup{
			ContentHash: "h", Size: 2,
			Members: []duplicates.CloneMember{{NodeID: "a", SymbolPath: "pkg.A", Kind: "function", FilePath: "a.go"}},
		})
	}
	return out
}

func makeClusters(n int) []duplicates.Cluster {
	out := make([]duplicates.Cluster, 0, n)
	for range n {
		out = append(out, duplicates.Cluster{
			Tier: duplicates.TierExact, Size: 2,
			Members: []duplicates.CloneMember{{NodeID: "a", SymbolPath: "pkg.A", Kind: "function", FilePath: "a.go"}},
		})
	}
	return out
}

func TestFindClones_DefaultCap(t *testing.T) {
	finder := &fakeCloneFinder{groups: makeCloneGroups(defaultListLimit + 25)}
	resp := dispatchClones(t, finder, map[string]any{"repo_id": "r1", "branch": "main"})
	if len(resp.Groups) != defaultListLimit {
		t.Errorf("returned groups = %d, want default cap %d", len(resp.Groups), defaultListLimit)
	}
	if resp.Total != defaultListLimit+25 {
		t.Errorf("total = %d, want %d", resp.Total, defaultListLimit+25)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true when total exceeds cap")
	}
}

func TestFindClones_ExplicitLimit(t *testing.T) {
	finder := &fakeCloneFinder{groups: makeCloneGroups(10)}
	resp := dispatchClones(t, finder, map[string]any{"repo_id": "r1", "branch": "main", "limit": 3})
	if len(resp.Groups) != 3 {
		t.Errorf("returned groups = %d, want 3", len(resp.Groups))
	}
	if resp.Total != 10 {
		t.Errorf("total = %d, want 10", resp.Total)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true")
	}
}

func TestFindClones_NotTruncatedWhenUnderCap(t *testing.T) {
	finder := &fakeCloneFinder{groups: makeCloneGroups(2)}
	resp := dispatchClones(t, finder, map[string]any{"repo_id": "r1", "branch": "main"})
	if len(resp.Groups) != 2 {
		t.Errorf("returned groups = %d, want 2", len(resp.Groups))
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.Truncated {
		t.Error("truncated = true, want false when under cap")
	}
}

// dispatchClusters dispatches eng_find_clusters and decodes the response.
func dispatchClusters(t *testing.T, finder CloneFinder, params map[string]any) FindClustersResponse {
	t.Helper()
	repos := &stubRepoLister{repos: []application.RepoRecord{{RepoID: "r1", ActiveBranch: "main"}}}
	r := NewRegistry()
	RegisterCloneTools(r, finder, repos)
	raw, _ := json.Marshal(params)
	req := &Request{Method: "eng_find_clusters", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	var resp FindClustersResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func TestFindClusters_DefaultCap(t *testing.T) {
	finder := &fakeCloneFinder{unified: makeClusters(defaultListLimit + 5)}
	resp := dispatchClusters(t, finder, map[string]any{"repo_id": "r1", "branch": "main"})
	if len(resp.Clusters) != defaultListLimit {
		t.Errorf("returned clusters = %d, want default cap %d", len(resp.Clusters), defaultListLimit)
	}
	if resp.Total != defaultListLimit+5 {
		t.Errorf("total = %d, want %d", resp.Total, defaultListLimit+5)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true")
	}
}

func TestFindClusters_ExplicitLimit(t *testing.T) {
	finder := &fakeCloneFinder{unified: makeClusters(8)}
	resp := dispatchClusters(t, finder, map[string]any{"repo_id": "r1", "branch": "main", "limit": 2})
	if len(resp.Clusters) != 2 {
		t.Errorf("returned clusters = %d, want 2", len(resp.Clusters))
	}
	if resp.Total != 8 {
		t.Errorf("total = %d, want 8", resp.Total)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true")
	}
}

func TestFindClusters_NotTruncatedWhenUnderCap(t *testing.T) {
	finder := &fakeCloneFinder{unified: makeClusters(3)}
	resp := dispatchClusters(t, finder, map[string]any{"repo_id": "r1", "branch": "main"})
	if len(resp.Clusters) != 3 {
		t.Errorf("returned clusters = %d, want 3", len(resp.Clusters))
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if resp.Truncated {
		t.Error("truncated = true, want false")
	}
}

// seedManyFindings inserts n open findings for the given repo/branch.
func seedManyFindings(t *testing.T, db *sql.DB, repoID, branch string, n int) {
	t.Helper()
	for i := range n {
		id := "f-" + string(rune('a'+i%26)) + "-" + time.Now().Format("150405.000000000") + "-" + string(rune('A'+i/26))
		seedFinding(t, db, id, branch, repoID, "low", "open")
	}
}

// dispatchListFindingsRaw dispatches eng_list_findings allowing arbitrary param value types (e.g. integer limit).
func dispatchListFindingsRaw(t *testing.T, r *Registry, params map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	req := &Request{JSONRPC: "2.0", Method: "eng_list_findings", Params: raw}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	return m
}

func TestListFindings_DefaultCap(t *testing.T) {
	db := newFindingsDB(t)
	seedManyFindings(t, db, "repo-1", "main", defaultListLimit+7)
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	m := dispatchListFindingsRaw(t, r, map[string]any{"repo_id": "repo-1", "branch": "main"})
	items, _ := m["findings"].([]findingRow)
	if len(items) != defaultListLimit {
		t.Errorf("returned findings = %d, want default cap %d", len(items), defaultListLimit)
	}
	if total, _ := m["total"].(int); total != defaultListLimit+7 {
		t.Errorf("total = %v, want %d", m["total"], defaultListLimit+7)
	}
	if trunc, _ := m["truncated"].(bool); !trunc {
		t.Error("truncated = false, want true")
	}
}

func TestListFindings_ExplicitLimit(t *testing.T) {
	db := newFindingsDB(t)
	seedManyFindings(t, db, "repo-1", "main", 12)
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	m := dispatchListFindingsRaw(t, r, map[string]any{"repo_id": "repo-1", "branch": "main", "limit": 4})
	items, _ := m["findings"].([]findingRow)
	if len(items) != 4 {
		t.Errorf("returned findings = %d, want 4", len(items))
	}
	if total, _ := m["total"].(int); total != 12 {
		t.Errorf("total = %v, want 12", m["total"])
	}
	if trunc, _ := m["truncated"].(bool); !trunc {
		t.Error("truncated = false, want true")
	}
}

func TestListFindings_NotTruncatedWhenUnderCap(t *testing.T) {
	db := newFindingsDB(t)
	seedManyFindings(t, db, "repo-1", "main", 3)
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	m := dispatchListFindingsRaw(t, r, map[string]any{"repo_id": "repo-1", "branch": "main"})
	items, _ := m["findings"].([]findingRow)
	if len(items) != 3 {
		t.Errorf("returned findings = %d, want 3", len(items))
	}
	if total, _ := m["total"].(int); total != 3 {
		t.Errorf("total = %v, want 3", m["total"])
	}
	if trunc, _ := m["truncated"].(bool); trunc {
		t.Error("truncated = true, want false")
	}
}
