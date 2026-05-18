package changedsymbols_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// memFiles maps "ref:path" to file content; a missing key models a file
// absent at that ref.
type memFiles map[string]string

func (m memFiles) changedFiles(_ context.Context, _, _, _ string) ([]string, error) {
	return []string{"code.go"}, nil
}

func (m memFiles) fileAtRef(_ context.Context, _, ref, path string) ([]byte, error) {
	if c, ok := m[ref+":"+path]; ok {
		return []byte(c), nil
	}
	return nil, errors.New("not present at ref")
}

func newService(t *testing.T, m memFiles) *changedsymbols.Service {
	t.Helper()
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), m.changedFiles, m.fileAtRef)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestDiff_ClassifiesAddedRemovedModified(t *testing.T) {
	m := memFiles{
		"refA:code.go": "package p\nfunc Keep() {}\nfunc Gone() {}\nfunc Edit() {}\n",
		"refB:code.go": "package p\nfunc Keep() {}\nfunc Edit() { _ = 1 }\nfunc Fresh() {}\n",
	}
	svc := newService(t, m)
	res, err := svc.Diff(context.Background(), "repo1", "/root", "refA", "refB")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !hasName(res.Added, "Fresh") {
		t.Errorf("expected Fresh added, got %+v", res.Added)
	}
	if !hasName(res.Removed, "Gone") {
		t.Errorf("expected Gone removed, got %+v", res.Removed)
	}
	if !hasName(res.Modified, "Edit") {
		t.Errorf("expected Edit modified, got %+v", res.Modified)
	}
	if hasName(res.Modified, "Keep") {
		t.Errorf("Keep is unchanged, should not be modified: %+v", res.Modified)
	}
}

func TestDiff_FileAddedAtRefB(t *testing.T) {
	// code.go absent at refA -> every symbol at refB is "added".
	m := memFiles{
		"refB:code.go": "package p\nfunc B() {}\n",
	}
	svc := newService(t, m)
	res, err := svc.Diff(context.Background(), "repo1", "/root", "refA", "refB")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !hasName(res.Added, "B") {
		t.Errorf("expected B added, got %+v", res.Added)
	}
	if len(res.Removed) != 0 || len(res.Modified) != 0 {
		t.Errorf("expected only additions, got removed=%v modified=%v", res.Removed, res.Modified)
	}
}

func TestNewService_RejectsNilDeps(t *testing.T) {
	m := memFiles{}
	if _, err := changedsymbols.NewService(nil, m.changedFiles, m.fileAtRef); err == nil {
		t.Error("expected error for nil parser")
	}
	if _, err := changedsymbols.NewService(treesitter.NewGoParser(), nil, m.fileAtRef); err == nil {
		t.Error("expected error for nil changedFiles")
	}
	if _, err := changedsymbols.NewService(treesitter.NewGoParser(), m.changedFiles, nil); err == nil {
		t.Error("expected error for nil fileAtRef")
	}
}

func hasName(cs []changedsymbols.SymbolChange, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}
