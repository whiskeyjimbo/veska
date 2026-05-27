package mcp

import (
	"context"
	"encoding/json"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestCheckRequired_ReportsAllMissing pins solov2-d2x: a call missing several
// required params learns all of them from one error, not one at a time.
func TestCheckRequired_ReportsAllMissing(t *testing.T) {
	t.Run("all present", func(t *testing.T) {
		if err := checkRequired("repo_id", "r", "branch", "main"); err != nil {
			t.Fatalf("expected nil, got %+v", err)
		}
	})

	t.Run("single missing names it", func(t *testing.T) {
		err := checkRequired("repo_id", "", "branch", "main")
		if err == nil || err.Code != CodeInvalidParams {
			t.Fatalf("expected InvalidParams, got %+v", err)
		}
		if err.Message != "repo_id is required" {
			t.Errorf("got %q", err.Message)
		}
	})

	t.Run("multiple missing lists all", func(t *testing.T) {
		err := checkRequired("query", "", "repo_id", "", "branch", "main")
		if err == nil {
			t.Fatal("expected error")
			return
		}
		if err.Message != "missing required params: query, repo_id" {
			t.Errorf("expected both names in one message, got %q", err.Message)
		}
	})
}

// scopedGraphStub is a minimal ports.GraphStorage that scopes nodes by
// (repoID, branch) — unlike stubGraphStorage which is global. Used by the
// fan-out tests so the helper actually has to walk multiple repos to find
// the seed's owner.
type scopedGraphStub struct {
	nodes map[string]map[string]*domain.Node // key1=repoID|branch, key2=nodeID
}

func newScopedGraphStub() *scopedGraphStub {
	return &scopedGraphStub{nodes: map[string]map[string]*domain.Node{}}
}

func (s *scopedGraphStub) put(repoID, branch string, n *domain.Node) {
	k := repoID + "|" + branch
	if s.nodes[k] == nil {
		s.nodes[k] = map[string]*domain.Node{}
	}
	s.nodes[k][string(n.ID)] = n
}

func (s *scopedGraphStub) SaveNode(_ context.Context, _, _ string, _ *domain.Node) error { return nil }
func (s *scopedGraphStub) SaveEdge(_ context.Context, _, _ string, _ *domain.Edge) error { return nil }
func (s *scopedGraphStub) DeleteFile(_ context.Context, _, _, _ string) error            { return nil }
func (s *scopedGraphStub) LoadGraph(_ context.Context, repoID, branch string) (*domain.Graph, error) {
	g, err := domain.NewGraph(repoID, branch)
	if err != nil {
		return nil, err
	}
	for _, n := range s.nodes[repoID+"|"+branch] {
		_ = g.AddNode(n)
	}
	return g, nil
}
func (s *scopedGraphStub) FindNodes(_ context.Context, repoID, branch, name string) ([]*domain.Node, error) {
	var out []*domain.Node
	for _, n := range s.nodes[repoID+"|"+branch] {
		if n.Name == name {
			out = append(out, n)
		}
	}
	return out, nil
}
func (s *scopedGraphStub) GetNode(_ context.Context, repoID, branch string, id domain.NodeID) (*domain.Node, error) {
	if m := s.nodes[repoID+"|"+branch]; m != nil {
		if n, ok := m[string(id)]; ok {
			return n, nil
		}
	}
	return nil, nil
}
func (s *scopedGraphStub) FindNodeByID(_ context.Context, id domain.NodeID) (*domain.Node, error) {
	for _, m := range s.nodes {
		if n, ok := m[string(id)]; ok {
			return n, nil
		}
	}
	return nil, nil
}
func (s *scopedGraphStub) NodesForFile(_ context.Context, repoID, branch, path string) ([]*domain.Node, error) {
	var out []*domain.Node
	for _, n := range s.nodes[repoID+"|"+branch] {
		if n.Path == path {
			out = append(out, n)
		}
	}
	return out, nil
}

// TestResolveSeedOwner_FanoutFindsUniqueOwner pins solov2-f0zt: when repo_id
// is omitted and multiple repos are registered, the seed must be located
// across all of them and resolve to the single owner — matching the
// "default: fan out across registered repos" documented in `veska
// calls/blast --help`. The previous code path errored "repo_id is required"
// despite the help text promising fan-out.
func TestResolveSeedOwner_FanoutFindsUniqueOwner(t *testing.T) {
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "repo-cli", ActiveBranch: "main"},
		{RepoID: "repo-lib", ActiveBranch: "main"},
	}}
	graph := newScopedGraphStub()
	hello := mustNode(t, "n-hello", "greet.go", "Hello", domain.KindMethod)
	graph.put("repo-lib", "main", hello)

	t.Run("by symbol", func(t *testing.T) {
		rid, br, nid, rpcErr := resolveSeedOwner(context.Background(), repos, graph, json.RawMessage(`{}`), "", "", "", "Hello")
		if rpcErr != nil {
			t.Fatalf("unexpected error: %+v", rpcErr)
		}
		if rid != "repo-lib" || br != "main" || nid != "n-hello" {
			t.Fatalf("got (%q,%q,%q); want (repo-lib,main,n-hello)", rid, br, nid)
		}
	})

	t.Run("by node_id", func(t *testing.T) {
		rid, _, nid, rpcErr := resolveSeedOwner(context.Background(), repos, graph, json.RawMessage(`{}`), "", "", "n-hello", "")
		if rpcErr != nil {
			t.Fatalf("unexpected error: %+v", rpcErr)
		}
		if rid != "repo-lib" || nid != "n-hello" {
			t.Fatalf("got (%q,%q); want (repo-lib,n-hello)", rid, nid)
		}
	})
}

// TestResolveSeedOwner_FanoutAmbiguousAcrossRepos pins the disambiguation
// path: if the same symbol exists in two repos, the helper must refuse
// rather than guess, and the error message must name each candidate so the
// caller can retry with --repo.
func TestResolveSeedOwner_FanoutAmbiguousAcrossRepos(t *testing.T) {
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "repo-a", ActiveBranch: "main"},
		{RepoID: "repo-b", ActiveBranch: "main"},
	}}
	graph := newScopedGraphStub()
	graph.put("repo-a", "main", mustNode(t, "n-a", "a.go", "Run", domain.KindFunction))
	graph.put("repo-b", "main", mustNode(t, "n-b", "b.go", "Run", domain.KindFunction))

	_, _, _, rpcErr := resolveSeedOwner(context.Background(), repos, graph, json.RawMessage(`{}`), "", "", "", "Run")
	if rpcErr == nil {
		t.Fatal("expected ambiguity error")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("want code %d, got %d (%q)", CodeInvalidParams, rpcErr.Code, rpcErr.Message)
	}
}

// TestResolveSeedOwner_FanoutNotFound pins the not-found path so a typo
// surfaces as a loud error rather than a silently-empty BFS result.
func TestResolveSeedOwner_FanoutNotFound(t *testing.T) {
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "repo-a", ActiveBranch: "main"},
		{RepoID: "repo-b", ActiveBranch: "main"},
	}}
	graph := newScopedGraphStub()
	_, _, _, rpcErr := resolveSeedOwner(context.Background(), repos, graph, json.RawMessage(`{}`), "", "", "", "Nope")
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("want NotFound, got %+v", rpcErr)
	}
}
