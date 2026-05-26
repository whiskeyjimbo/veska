package dependencies_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
)

type stubAggregator struct {
	rows []dependencies.StubRow
	err  error
}

func (s *stubAggregator) AggregateStubs(_ context.Context, _, _ string) ([]dependencies.StubRow, error) {
	return s.rows, s.err
}

func TestService_GroupsByModuleAndCountsCallSites(t *testing.T) {
	agg := &stubAggregator{rows: []dependencies.StubRow{
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "Command", SrcNodeID: "a", Language: "go"},
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "Execute", SrcNodeID: "b", Language: "go"},
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "AddCommand", SrcNodeID: "c", Language: "go"},
		{ModulePath: "golang.org/x/mod", SymbolPath: "Parse", SrcNodeID: "d", Language: "go"},
	}}
	svc, err := dependencies.NewService(agg, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.List(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Dependencies) != 2 {
		t.Fatalf("want 2 modules, got %d: %+v", len(res.Dependencies), res.Dependencies)
	}
	// Sorted by usage_count desc: cobra (3) before mod (1).
	if res.Dependencies[0].Module != "github.com/spf13/cobra" || res.Dependencies[0].UsageCount != 3 {
		t.Errorf("first: %+v want cobra/3", res.Dependencies[0])
	}
	if res.Dependencies[1].Module != "golang.org/x/mod" || res.Dependencies[1].UsageCount != 1 {
		t.Errorf("second: %+v want mod/1", res.Dependencies[1])
	}
	if len(res.Dependencies[0].TopCallSites) != 3 {
		t.Errorf("cobra TopCallSites want 3, got %d", len(res.Dependencies[0].TopCallSites))
	}
}

func TestService_DefaultTopKCapsCallSites(t *testing.T) {
	rows := make([]dependencies.StubRow, 0, dependencies.DefaultTopK+3)
	for i := 0; i < dependencies.DefaultTopK+3; i++ {
		rows = append(rows, dependencies.StubRow{
			ModulePath: "github.com/foo/bar",
			SymbolPath: "Func",
			SrcNodeID:  string(rune('a' + i)),
			Language:   "go",
		})
	}
	svc, _ := dependencies.NewService(&stubAggregator{rows: rows}, nil, nil)
	res, _ := svc.List(context.Background(), "r1", "main")
	if len(res.Dependencies) != 1 {
		t.Fatalf("want 1 module, got %d", len(res.Dependencies))
	}
	if got := len(res.Dependencies[0].TopCallSites); got != dependencies.DefaultTopK {
		t.Errorf("TopCallSites len = %d, want %d", got, dependencies.DefaultTopK)
	}
	if res.Dependencies[0].UsageCount != dependencies.DefaultTopK+3 {
		t.Errorf("UsageCount = %d, want %d", res.Dependencies[0].UsageCount, dependencies.DefaultTopK+3)
	}
}

func TestService_ResolvesVersionsWhenWired(t *testing.T) {
	agg := &stubAggregator{rows: []dependencies.StubRow{
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "Command", SrcNodeID: "a", Language: "go"},
	}}
	versions := func(_ context.Context, _, mod string) (string, error) {
		if mod == "github.com/spf13/cobra" {
			return "v1.10.2", nil
		}
		return "", nil
	}
	repoRoot := func(_ context.Context, _ string) (string, error) { return "/repo", nil }
	svc, _ := dependencies.NewService(agg, versions, repoRoot)
	res, err := svc.List(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Dependencies[0].Version != "v1.10.2" {
		t.Errorf("Version = %q, want v1.10.2", res.Dependencies[0].Version)
	}
}

func TestService_EmptyAggregateReturnsNonNilSlice(t *testing.T) {
	svc, _ := dependencies.NewService(&stubAggregator{}, nil, nil)
	res, err := svc.List(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// JSON marshaling guarantee: callers must see [] not null.
	if res.Dependencies == nil {
		t.Errorf("Dependencies must be non-nil slice")
	}
	if len(res.Dependencies) != 0 {
		t.Errorf("want 0 deps, got %d", len(res.Dependencies))
	}
}

func TestService_NilAggregatorRejected(t *testing.T) {
	_, err := dependencies.NewService(nil, nil, nil)
	if err == nil {
		t.Fatal("want error for nil aggregator")
	}
	if !errors.Is(err, dependencies.ErrMissingDependency) {
		t.Errorf("want ErrMissingDependency, got %v", err)
	}
}

func TestService_AggregatorErrorPropagates(t *testing.T) {
	agg := &stubAggregator{err: errors.New("db down")}
	svc, _ := dependencies.NewService(agg, nil, nil)
	_, err := svc.List(context.Background(), "r1", "main")
	if err == nil {
		t.Fatal("want error")
	}
}
