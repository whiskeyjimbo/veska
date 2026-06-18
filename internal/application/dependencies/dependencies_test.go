// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package dependencies_test

import (
	"context"
	"errors"
	"fmt"
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

type importLister struct {
	rows []dependencies.ImportRow
	err  error
}

func (l *importLister) ListImports(_ context.Context, _, _ string) ([]dependencies.ImportRow, error) {
	return l.rows, l.err
}

// TestService_UnionsImportsWithStubs pins: a module imported
// in N files but with zero resolved CALLS still surfaces with UsageCount=0
// and ImportCount=N. The case mirrors the junior-journey reproduction: a
// vanilla cobra-based CLI lists github.com/spf13/cobra in deps even
// before `go mod vendor` (no resolved CALLS yet).
func TestService_UnionsImportsWithStubs(t *testing.T) {
	agg := &stubAggregator{rows: []dependencies.StubRow{
		{ModulePath: "github.com/junior/greetlib", SymbolPath: "Farewell", SrcNodeID: "a", Language: "go"},
	}}
	imps := &importLister{rows: []dependencies.ImportRow{
		// greetlib is both imported AND called.
		{FilePath: "cmd/root.go", ImportPath: "github.com/junior/greetlib", Language: "go"},
		// cobra is imported in two files but never resolved as a CALLS edge.
		{FilePath: "cmd/root.go", ImportPath: "github.com/spf13/cobra", Language: "go"},
		{FilePath: "cmd/sub.go", ImportPath: "github.com/spf13/cobra", Language: "go"},
	}}
	svc, err := dependencies.NewService(agg, nil, nil, dependencies.WithImportLister(imps))
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
	// greetlib: UsageCount=1 (stub) and ImportCount=1 → ranks first.
	if got := res.Dependencies[0]; got.Module != "github.com/junior/greetlib" || got.UsageCount != 1 || got.ImportCount != 1 {
		t.Errorf("first = %+v, want greetlib UC=1 IC=1", got)
	}
	// cobra: UsageCount=0 ImportCount=2 → present, ranks last.
	if got := res.Dependencies[1]; got.Module != "github.com/spf13/cobra" || got.UsageCount != 0 || got.ImportCount != 2 {
		t.Errorf("second = %+v, want cobra UC=0 IC=2", got)
	}
	if len(res.Dependencies[1].TopCallSites) != 0 {
		t.Errorf("cobra TopCallSites = %v, want empty (no stubs)", res.Dependencies[1].TopCallSites)
	}
}

// TestService_ImportCountDistinctByFile pins that repeated imports of the
// same module from the same file count as one (file-distinct, not row
// count). Prevents accidental double-counting on re-promote.
func TestService_ImportCountDistinctByFile(t *testing.T) {
	imps := &importLister{rows: []dependencies.ImportRow{
		{FilePath: "cmd/root.go", ImportPath: "github.com/spf13/cobra", Language: "go"},
		{FilePath: "cmd/root.go", ImportPath: "github.com/spf13/cobra", Language: "go"},
	}}
	svc, _ := dependencies.NewService(&stubAggregator{}, nil, nil, dependencies.WithImportLister(imps))
	res, _ := svc.List(context.Background(), "r1", "main")
	if len(res.Dependencies) != 1 || res.Dependencies[0].ImportCount != 1 {
		t.Errorf("want 1 module with ImportCount=1; got %+v", res.Dependencies)
	}
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
	for i := range dependencies.DefaultTopK + 3 {
		rows = append(rows, dependencies.StubRow{
			ModulePath: "github.com/foo/bar",
			SymbolPath: fmt.Sprintf("Func%d", i),
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

// TestService_DedupesTopCallSitesBySymbol pins: when N stub
// rows all target the same SymbolPath (the common case for a hot library
// function called from many sites), TopCallSites must show that symbol
// exactly once and use the remaining TopK slots for other symbols.
// Previously the output was "New, Hello, New, Shout" - the same name
// duplicated across the sample.
func TestService_DedupesTopCallSitesBySymbol(t *testing.T) {
	rows := []dependencies.StubRow{
		{ModulePath: "m", SymbolPath: "New", SrcNodeID: "a", Language: "go"},
		{ModulePath: "m", SymbolPath: "Hello", SrcNodeID: "b", Language: "go"},
		{ModulePath: "m", SymbolPath: "New", SrcNodeID: "c", Language: "go"},
		{ModulePath: "m", SymbolPath: "Shout", SrcNodeID: "d", Language: "go"},
	}
	svc, _ := dependencies.NewService(&stubAggregator{rows: rows}, nil, nil)
	res, _ := svc.List(context.Background(), "r1", "main")
	if len(res.Dependencies) != 1 {
		t.Fatalf("want 1 module, got %d", len(res.Dependencies))
	}
	got := res.Dependencies[0].TopCallSites
	seen := map[string]int{}
	for _, cs := range got {
		seen[cs.SymbolPath]++
	}
	for sym, n := range seen {
		if n > 1 {
			t.Errorf("symbol %q appears %d times in TopCallSites; want distinct", sym, n)
		}
	}
	if res.Dependencies[0].UsageCount != 4 {
		t.Errorf("UsageCount = %d, want 4 (every stub row counts toward popularity)", res.Dependencies[0].UsageCount)
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
	// JSON marshaling guarantee: callers must see not null.
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
