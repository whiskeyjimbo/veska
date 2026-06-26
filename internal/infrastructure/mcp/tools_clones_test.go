// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type fakeCloneFinder struct {
	groups         []duplicates.CloneGroup
	clusters       []duplicates.NearCluster
	unified        []duplicates.Cluster
	gotMinScore    float32
	nearCalled     bool
	gotClusterOpts duplicates.ClusterOptions
}

func (f *fakeCloneFinder) ExactClones(_ context.Context, _, _ string) ([]duplicates.CloneGroup, error) {
	return f.groups, nil
}

func (f *fakeCloneFinder) NearDuplicates(_ context.Context, _, _ string, minScore float32) ([]duplicates.NearCluster, error) {
	f.nearCalled = true
	f.gotMinScore = minScore
	return f.clusters, nil
}

func (f *fakeCloneFinder) Clusters(_ context.Context, opts duplicates.ClusterOptions) ([]duplicates.Cluster, error) {
	f.gotClusterOpts = opts
	return f.unified, nil
}

// registerDupesForTest registers the merged eng_find_duplicates tool with only
// the clone finder wired (search-seed deps nil, exercised elsewhere), for the
// clones/clusters seed tests.
func registerDupesForTest(finder CloneFinder, repos application.RepoLister) *Registry {
	r := NewRegistry()
	RegisterDuplicatesTool(r, finder, nil, nil, nil, repos, nil)
	return r
}

// dupRequest builds an eng_find_duplicates request with the given seed injected.
func dupRequest(t *testing.T, seed string, params map[string]any) *Request {
	t.Helper()
	out := map[string]any{"seed": seed}
	for k, v := range params {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	return &Request{Method: "eng_find_duplicates", Params: json.RawMessage(raw)}
}

func dispatchClones(t *testing.T, finder CloneFinder, params map[string]any) FindClonesResponse {
	t.Helper()
	repos := &stubRepoLister{repos: []application.RepoRecord{{RepoID: "r1", ActiveBranch: "main"}}}
	r := registerDupesForTest(finder, repos)
	req := dupRequest(t, "clones", params)
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	var resp FindClonesResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func TestFindClones_ExactDefault(t *testing.T) {
	finder := &fakeCloneFinder{groups: []duplicates.CloneGroup{
		{ContentHash: "h", Size: 2, Members: []duplicates.CloneMember{
			{NodeID: "a", SymbolPath: "pkg.A", Kind: "function", FilePath: "a.go"},
			{NodeID: "b", SymbolPath: "pkg.B", Kind: "function", FilePath: "b.go"},
		}},
	}}
	resp := dispatchClones(t, finder, map[string]any{"repo_id": "r1", "branch": "main"})
	if resp.Mode != "exact" {
		t.Errorf("mode = %q, want exact", resp.Mode)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].Size != 2 {
		t.Fatalf("groups = %+v, want one group of size 2", resp.Groups)
	}
	if resp.Clusters == nil {
		t.Errorf("clusters must serialize as [] not null")
	}
	if finder.nearCalled {
		t.Errorf("exact mode must not call NearDuplicates")
	}
}

func TestFindClones_NearMode(t *testing.T) {
	finder := &fakeCloneFinder{clusters: []duplicates.NearCluster{
		{Size: 2, MinScore: 0.82, MaxScore: 0.91, Members: []duplicates.CloneMember{
			{NodeID: "a", SymbolPath: "pkg.A", Kind: "function", FilePath: "a.go"},
			{NodeID: "b", SymbolPath: "pkg.B", Kind: "function", FilePath: "b.go"},
		}},
	}}
	resp := dispatchClones(t, finder, map[string]any{"repo_id": "r1", "branch": "main", "mode": "near", "min_score": 0.75})
	if resp.Mode != "near" {
		t.Errorf("mode = %q, want near", resp.Mode)
	}
	if !finder.nearCalled {
		t.Fatalf("near mode must call NearDuplicates")
	}
	if finder.gotMinScore != 0.75 {
		t.Errorf("forwarded min_score = %v, want 0.75", finder.gotMinScore)
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].MinScore != 0.82 {
		t.Fatalf("clusters = %+v, want one cluster minScore 0.82", resp.Clusters)
	}
	if resp.Groups == nil {
		t.Errorf("groups must serialize as [] not null")
	}
}

func TestFindClones_InvalidMode(t *testing.T) {
	repos := &stubRepoLister{repos: []application.RepoRecord{{RepoID: "r1", ActiveBranch: "main"}}}
	r := registerDupesForTest(&fakeCloneFinder{}, repos)
	req := dupRequest(t, "clones", map[string]any{"repo_id": "r1", "mode": "bogus"})
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr == nil {
		t.Fatalf("expected an error for invalid mode")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %v, want CodeInvalidParams", rpcErr.Code)
	}
}
