// SPDX-License-Identifier: AGPL-3.0-only

package extindex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

type fakeSaver struct {
	calls []*domain.Node
	err   error
}

func (f *fakeSaver) SaveExternalNode(_ context.Context, _, _ string, n *domain.Node) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, n)
	return nil
}

// writeVendorFixture creates a vendor/<modulePath> tree under root
// with two.go files (one.go, one _test.go which the indexer must
// skip).
func writeVendorFixture(t *testing.T, root, modulePath, exportedSrc, testSrc string) {
	t.Helper()
	dir := filepath.Join(root, "vendor", modulePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "greet.go"), []byte(exportedSrc), 0o644); err != nil {
		t.Fatalf("write greet.go: %v", err)
	}
	if testSrc != "" {
		if err := os.WriteFile(filepath.Join(dir, "greet_test.go"), []byte(testSrc), 0o644); err != nil {
			t.Fatalf("write _test.go: %v", err)
		}
	}
}

func TestIndexVendorModule_ExtractsExportedSymbols(t *testing.T) {
	root := t.TempDir()
	writeVendorFixture(t, root, "github.com/example/greetlib", `package greetlib

type Greeter struct{ Prefix string }
func New(prefix string) *Greeter { return &Greeter{Prefix: prefix} }
func (g *Greeter) Hello(name string) string { return g.Prefix + ", " + name + "!" }
`, "")

	saver := &fakeSaver{}
	svc, err := extindex.NewService(treesitter.NewGoParser(), saver)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.IndexVendorModule(context.Background(), "repo1", "main", root, "github.com/example/greetlib")
	if err != nil {
		t.Fatalf("IndexVendorModule: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 (greet.go)", res.Files)
	}
	// Expected nodes: package greetlib + struct Greeter + func New + method Greeter.Hello (+ implicit interface methods or chunks may add extras; check for presence of the headline names).
	wantNames := map[string]bool{"greetlib": false, "Greeter": false, "New": false, "Greeter.Hello": false}
	for _, n := range saver.calls {
		if _, ok := wantNames[n.Name]; ok {
			wantNames[n.Name] = true
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("expected indexed node %q, got names: %v", name, namesOf(saver.calls))
		}
	}
}

func TestIndexVendorModule_SkipsTestFiles(t *testing.T) {
	root := t.TempDir()
	writeVendorFixture(t, root, "github.com/example/lib", `package lib
func Prod() {}
`, `package lib

func TestProd(*testingT) {}

type testingT struct{}
`)
	saver := &fakeSaver{}
	svc, _ := extindex.NewService(treesitter.NewGoParser(), saver)
	res, err := svc.IndexVendorModule(context.Background(), "repo1", "main", root, "github.com/example/lib")
	if err != nil {
		t.Fatalf("IndexVendorModule: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1 (greet_test.go must be skipped)", res.Files)
	}
	for _, n := range saver.calls {
		if n.Name == "TestProd" || n.Name == "testingT" {
			t.Errorf("test-file symbol leaked into indexed nodes: %q", n.Name)
		}
	}
}

func TestIndexVendorModule_ModuleNotVendored(t *testing.T) {
	root := t.TempDir()
	// No vendor/ tree - the indexer should report ErrModuleNotVendored.
	saver := &fakeSaver{}
	svc, _ := extindex.NewService(treesitter.NewGoParser(), saver)
	_, err := svc.IndexVendorModule(context.Background(), "repo1", "main", root, "github.com/example/lib")
	if err == nil {
		t.Fatal("want ErrModuleNotVendored, got nil")
	}
	if !errors.Is(err, extindex.ErrModuleNotVendored) {
		t.Errorf("want ErrModuleNotVendored, got %v", err)
	}
}

func TestNewService_RejectsNilDeps(t *testing.T) {
	if _, err := extindex.NewService(nil, &fakeSaver{}); err == nil {
		t.Error("want error for nil parser")
	}
	if _, err := extindex.NewService(treesitter.NewGoParser(), nil); err == nil {
		t.Error("want error for nil saver")
	}
}

func namesOf(ns []*domain.Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}
