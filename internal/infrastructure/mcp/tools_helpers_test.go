package mcp

import (
	"context"
	"encoding/json"
	"strings"
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
func (s *scopedGraphStub) GetNodeSnippet(_ context.Context, repoID, branch string, id domain.NodeID) (string, error) {
	if m, ok := s.nodes[repoID+"|"+branch]; ok {
		if n, ok := m[string(id)]; ok && n.RawContent != nil {
			return *n.RawContent, nil
		}
	}
	return "", nil
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

// TestResolveSeedOwner_CwdPinFallsThroughToFanout pins solov2-izh6.14: when
// the MCP shim injects a cwd that resolves to a registered repo but the seed
// symbol/node_id does NOT live in that repo, the helper must fall through
// to the fan-out path (Path 3) instead of returning NotFound. Otherwise
// `veska calls Hello` from a sibling repo fails with "symbol not found"
// despite Hello being registered elsewhere — contradicting the documented
// "default: fan out across registered repos" contract.
func TestResolveSeedOwner_CwdPinFallsThroughToFanout(t *testing.T) {
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "repo-cli", RootPath: "/tmp/cli", ActiveBranch: "main"},
		{RepoID: "repo-lib", RootPath: "/tmp/lib", ActiveBranch: "main"},
	}}
	graph := newScopedGraphStub()
	hello := mustNode(t, "n-hello", "greet.go", "Hello", domain.KindMethod)
	graph.put("repo-lib", "main", hello)

	// cwd is the CLI repo; the symbol lives in the lib repo. Without the
	// fallthrough fix, this is the failing path that produced
	// "symbol not found: Hello".
	params := json.RawMessage(`{"cwd":"/tmp/cli"}`)

	t.Run("by symbol", func(t *testing.T) {
		rid, br, nid, rpcErr := resolveSeedOwner(context.Background(), repos, graph, params, "", "", "", "Hello")
		if rpcErr != nil {
			t.Fatalf("unexpected error: %+v", rpcErr)
		}
		if rid != "repo-lib" || br != "main" || nid != "n-hello" {
			t.Fatalf("got (%q,%q,%q); want (repo-lib,main,n-hello)", rid, br, nid)
		}
	})

	t.Run("by node_id", func(t *testing.T) {
		rid, _, nid, rpcErr := resolveSeedOwner(context.Background(), repos, graph, params, "", "", "n-hello", "")
		if rpcErr != nil {
			t.Fatalf("unexpected error: %+v", rpcErr)
		}
		if rid != "repo-lib" || nid != "n-hello" {
			t.Fatalf("got (%q,%q); want (repo-lib,n-hello)", rid, nid)
		}
	})
}

// TestExpandNodeIDPrefix_RejectsBadAndExpandsGood pins solov2-xc7t: when a
// caller passes a 12-char short_id (the form veska's CLI prints under the
// "(...)" column), the daemon must NOT silently pass it through to a SQL
// equality lookup and report "node has no embedding" — instead either
// expand it to the canonical 64-char form (unique prefix), or surface a
// "no node matches prefix" error so the user knows the id was wrong.
func TestExpandNodeIDPrefix_RejectsBadAndExpandsGood(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	g := newScopedGraphStub()
	n, err := domain.NewNode(full, "x.go", "X", domain.KindFunction)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	g.put("r1", "main", n)

	t.Run("full id passes through unchanged", func(t *testing.T) {
		got, rpcErr := expandNodeIDPrefix(context.Background(), g, "r1", "main", full)
		if rpcErr != nil || got != full {
			t.Fatalf("got (%q, %+v); want (%q, nil)", got, rpcErr, full)
		}
	})

	t.Run("short prefix expands to full", func(t *testing.T) {
		got, rpcErr := expandNodeIDPrefix(context.Background(), g, "r1", "main", full[:12])
		if rpcErr != nil {
			t.Fatalf("unexpected error: %+v", rpcErr)
		}
		if got != full {
			t.Errorf("got %q, want %q", got, full)
		}
	})

	t.Run("unknown prefix surfaces NotFound", func(t *testing.T) {
		_, rpcErr := expandNodeIDPrefix(context.Background(), g, "r1", "main", "deadbeef")
		if rpcErr == nil || rpcErr.Code != CodeNotFound {
			t.Errorf("want NotFound, got %+v", rpcErr)
		}
	})
}

// TestSortedKeysAnnotated_FlagsRequired pins solov2-m5c2: when a tool's
// schema declares "required" properties, the unknown-parameter error must
// mark them as such so a caller can correct the call from the error
// alone. Required keys also sort before optional ones.
func TestSortedKeysAnnotated_FlagsRequired(t *testing.T) {
	props := map[string]json.RawMessage{
		"file_path": json.RawMessage(`{}`),
		"line":      json.RawMessage(`{}`),
		"limit":     json.RawMessage(`{}`),
		"branch":    json.RawMessage(`{}`),
	}
	req := map[string]bool{"file_path": true, "line": true}
	got := sortedKeysAnnotated(props, req)
	want := "file_path (required), line (required), branch, limit"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestRequireRepoID_CountExcludesExtRepos pins solov2-dqga: the
// "N repos registered" hint in the missing-repo_id error must agree
// with what eng_list_repos shows by default, which hides synthetic
// ext:<module> rows produced by `veska deps index`. Before the fix
// the count came from raw len(all) and reported one more than the user
// could see in eng_list_repos, sending agents looking for a phantom
// repo. Junior-journey repro: 4 tracked repos + 1 deps-indexed cobra
// produced "5 repos registered" while eng_list_repos showed 4.
func TestRequireRepoID_CountExcludesExtRepos(t *testing.T) {
	repos := []application.RepoRecord{
		{RepoID: "tracked-1", RootPath: "/a"},
		{RepoID: "tracked-2", RootPath: "/b"},
		{RepoID: "ext:github.com/spf13/cobra", RootPath: "/a/vendor/github.com/spf13/cobra"},
	}
	if got, want := userVisibleRepoCount(repos), 2; got != want {
		t.Errorf("userVisibleRepoCount = %d, want %d", got, want)
	}

	lister := &stubRepoLister{repos: repos}
	_, rpcErr := resolveRepoIDOrCwd(context.Background(), lister, "", "")
	if rpcErr == nil {
		t.Fatal("want RPC error, got nil")
	}
	if !strings.Contains(rpcErr.Message, "2 repos registered") {
		t.Errorf("error must report user-visible count (2), got %q", rpcErr.Message)
	}
	if strings.Contains(rpcErr.Message, "3 repos registered") {
		t.Errorf("error must not include ext: rows in the count, got %q", rpcErr.Message)
	}
}
