package contextpack_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// --- blastradius fakes (same shape as blastradius_test) -------------------

type fakeEdges struct {
	inbound  map[string][]string
	outbound map[string][]string
}

func (f *fakeEdges) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *fakeEdges) OutboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.outbound[id]...)
	}
	return out, nil
}

type fakeNodes struct {
	metas  map[string]ports.NodeMeta
	byFile map[string][]string
}

func (f *fakeNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	var out []ports.NodeMeta
	for _, id := range ids {
		if m, ok := f.metas[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeNodes) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	return f.byFile[filePath], nil
}

// --- test fixture ---------------------------------------------------------

func newAssembler(t *testing.T, opts ...contextpack.Option) *contextpack.Assembler {
	t.Helper()
	edges := &fakeEdges{
		inbound: map[string][]string{
			"seed": {"caller1"},
		},
	}
	nodes := &fakeNodes{
		metas: map[string]ports.NodeMeta{
			"seed":    {NodeID: "seed", SymbolPath: "pkg.Target", FilePath: "a.go", Kind: "function"},
			"caller1": {NodeID: "caller1", SymbolPath: "pkg.Caller", FilePath: "b.go", Kind: "function"},
		},
		byFile: map[string][]string{
			"a.go": {"seed"},
			"b.go": {"caller1"},
		},
	}
	blast := blastradius.NewService(edges, nodes, nil)

	findNodes := func(_ context.Context, _, _, sym string) ([]*domain.Node, error) {
		if sym == "Target" {
			n, _ := domain.NewNode("seed", "a.go", "Target", domain.KindFunction)
			return []*domain.Node{n}, nil
		}
		return nil, nil
	}
	fileHistory := func(_ context.Context, _, path string, _ time.Duration) ([]contextpack.CommitInfo, error) {
		return []contextpack.CommitInfo{
			{Hash: "h-" + path, Author: "dev", When: time.Unix(1000, 0), Subject: "touch " + path},
		}, nil
	}
	openFindings := func(_ context.Context, _, _ string) (map[string]bool, error) {
		return map[string]bool{"caller1": true}, nil
	}
	changedFiles := func(_ context.Context, _ string) ([]string, error) {
		return []string{"a.go"}, nil
	}
	activeTask := func(_ context.Context, repoID string) (*contextpack.TaskInfo, error) {
		return &contextpack.TaskInfo{TaskID: "t1", RepoID: repoID, Title: "do work", Active: true}, nil
	}

	a, err := contextpack.NewAssembler(findNodes, blast, fileHistory, openFindings, changedFiles, nodes.NodesInFile, activeTask, opts...)
	if err != nil {
		t.Fatalf("NewAssembler: %v", err)
	}
	return a
}

// AC1 — symbol mode.
func TestForSymbol_BundlesAllSections(t *testing.T) {
	a := newAssembler(t)
	p, err := a.ForSymbol(context.Background(), "r", "main", "/repo", "Target")
	if err != nil {
		t.Fatalf("ForSymbol: %v", err)
	}
	if p.Mode != "symbol" || p.Query != "Target" {
		t.Fatalf("mode/query = %q/%q", p.Mode, p.Query)
	}
	if len(p.Nodes) != 2 {
		t.Fatalf("want 2 relevant nodes (seed+blast), got %d: %+v", len(p.Nodes), p.Nodes)
	}
	if len(p.RecentCommits) == 0 {
		t.Fatal("want recent commits")
	}
	if len(p.OpenFindings) != 1 || p.OpenFindings[0].NodeID != "caller1" {
		t.Fatalf("want 1 open finding on caller1, got %+v", p.OpenFindings)
	}
	if len(p.Tasks) != 1 || p.Tasks[0].TaskID != "t1" {
		t.Fatalf("want active task t1, got %+v", p.Tasks)
	}
}

// AC1 — task mode.
func TestForTask_BundlesFromChangedFiles(t *testing.T) {
	a := newAssembler(t)
	p, err := a.ForTask(context.Background(), "r", "main", "/repo", "t1")
	if err != nil {
		t.Fatalf("ForTask: %v", err)
	}
	if p.Mode != "task" || p.Query != "t1" {
		t.Fatalf("mode/query = %q/%q", p.Mode, p.Query)
	}
	// a.go contains "seed"; blast radius adds caller1.
	if len(p.Nodes) != 2 {
		t.Fatalf("want 2 nodes from changed-file seeds, got %d: %+v", len(p.Nodes), p.Nodes)
	}
}

// AC2 — oversized bundle truncated, not rejected.
func TestClip_TruncatesOversizedBundle(t *testing.T) {
	a := newAssembler(t, contextpack.WithTokenBudget(1))
	p, err := a.ForSymbol(context.Background(), "r", "main", "/repo", "Target")
	if err != nil {
		t.Fatalf("ForSymbol: %v", err)
	}
	if !p.Truncated {
		t.Fatal("want Truncated=true under a tiny budget")
	}
	// Lowest-priority sections dropped first: tasks and findings gone.
	if len(p.Tasks) != 0 {
		t.Fatalf("want tasks dropped first, got %+v", p.Tasks)
	}
}

func TestClip_WithinBudgetNotTruncated(t *testing.T) {
	a := newAssembler(t, contextpack.WithTokenBudget(contextpack.DefaultTokenBudget))
	p, err := a.ForSymbol(context.Background(), "r", "main", "/repo", "Target")
	if err != nil {
		t.Fatalf("ForSymbol: %v", err)
	}
	if p.Truncated {
		t.Fatal("typical bundle should fit the default budget")
	}
	if p.EstimatedTokens <= 0 {
		t.Fatalf("EstimatedTokens should be positive, got %d", p.EstimatedTokens)
	}
}

func TestNewAssembler_RejectsNilDependency(t *testing.T) {
	_, err := contextpack.NewAssembler(nil, nil, nil, nil, nil, nil, nil)
	if !errors.Is(err, contextpack.ErrMissingDependency) {
		t.Fatalf("want ErrMissingDependency, got %v", err)
	}
}

// AC3 — p95 latency under 50ms for a typical input.
func TestForSymbol_P95Latency(t *testing.T) {
	a := newAssembler(t)
	const iter = 200
	durs := make([]time.Duration, iter)
	for i := range durs {
		start := time.Now()
		if _, err := a.ForSymbol(context.Background(), "r", "main", "/repo", "Target"); err != nil {
			t.Fatalf("ForSymbol: %v", err)
		}
		durs[i] = time.Since(start)
	}
	p95 := percentile(durs, 95)
	if p95 > 50*time.Millisecond {
		t.Fatalf("p95 latency %v exceeds 50ms", p95)
	}
}

func percentile(d []time.Duration, p int) time.Duration {
	cp := append([]time.Duration(nil), d...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	idx := (p * len(cp)) / 100
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func TestSymbolLeaf_Untouched(t *testing.T) {
	// Guard: symbolLeaf behaviour is exercised via NodeInfo.Name.
	a := newAssembler(t)
	p, _ := a.ForSymbol(context.Background(), "r", "main", "/repo", "Target")
	for _, n := range p.Nodes {
		if strings.Contains(n.Name, ".") {
			t.Fatalf("node name %q should be the leaf segment", n.Name)
		}
	}
}
