package changedsymbols_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// memFiles maps "ref:path" to file content; a missing key models a file
// absent at that ref. Adapters must wrap the sentinel
// changedsymbols.ErrFileAbsentAtRef on legitimate absence so the Service
// can distinguish "file legitimately not present at this ref" from
// "couldn't read the ref at all".
type memFiles map[string]string

func (m memFiles) changedFiles(_ context.Context, _, _, _ string) ([]string, error) {
	return []string{"code.go"}, nil
}

func (m memFiles) fileAtRef(_ context.Context, _, ref, path string) ([]byte, error) {
	if c, ok := m[ref+":"+path]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("%w: %s:%s", changedsymbols.ErrFileAbsentAtRef, ref, path)
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

// multiFileFiles supports a per-test changed-file list and per-ref
// per-path content, plus a configurable "unreachable" error injected for
// arbitrary (ref,path) pairs so tests can simulate ref_a's tree being
// unreadable while ref_b succeeds.
type multiFileFiles struct {
	changed     []string
	contents    map[string]string // "ref:path" -> content; missing => absent (wraps sentinel)
	unreachable map[string]bool   // "ref:path" -> true => non-sentinel error
}

func (m *multiFileFiles) changedFilesFn(_ context.Context, _, _, _ string) ([]string, error) {
	return append([]string(nil), m.changed...), nil
}

func (m *multiFileFiles) fileAtRefFn(_ context.Context, _, ref, path string) ([]byte, error) {
	k := ref + ":" + path
	if m.unreachable[k] {
		return nil, errors.New("git: tree unreachable (simulated)")
	}
	if c, ok := m.contents[k]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("%w: %s", changedsymbols.ErrFileAbsentAtRef, k)
}

// when ref_a is unreadable (e.g. its tree isn't in the
// promotion store / can't be checked out) and ref_b has only
// non-symbol-yielding diffs, the result must surface
// "baseline_ref_not_indexed" rather than the misleading
// "non_symbol_changes_only".
func TestDiff_BaselineRefUnreachable_EmitsBaselineRefNotIndexed(t *testing.T) {
	m := &multiFileFiles{
		changed: []string{"go.mod"},
		contents: map[string]string{
			// go.mod isn't a Go source file the parser yields symbols for,
			// so the symbol diff is empty regardless. Without the
			// baseline-unreachable signal this would degrade as
			// "non_symbol_changes_only".
			"refB:go.mod": "module x\n",
		},
		unreachable: map[string]bool{"refA:go.mod": true},
	}
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), m.changedFilesFn, m.fileAtRefFn)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Diff(context.Background(), "repo1", "/root", "refA", "refB")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !containsReason(res.DegradedReasons, changedsymbols.DegradedReasonBaselineRefNotIndexed) {
		t.Errorf("expected degraded reason %q, got %v",
			changedsymbols.DegradedReasonBaselineRefNotIndexed, res.DegradedReasons)
	}
	if containsReason(res.DegradedReasons, changedsymbols.DegradedReasonNonSymbolChangesOnly) {
		t.Errorf("must not emit %q alongside baseline-not-indexed: %v",
			changedsymbols.DegradedReasonNonSymbolChangesOnly, res.DegradedReasons)
	}
}

// acceptance #2: when both refs are reachable and a new
// file containing a new symbol is added between them, the added symbol
// is returned — NOT classified as a non-symbol change. The existing
// classifier already does this, so the test guards against regressions
// from the baseline-unreachable refactor.
func TestDiff_NewFileWithNewSymbol_ClassifiedAsAdded(t *testing.T) {
	m := &multiFileFiles{
		changed: []string{"cmd/jwt.go"},
		contents: map[string]string{
			// File doesn't exist at refA (legitimately absent — sentinel
			// wrapped by the mock). At refB it contains a new function.
			"refB:cmd/jwt.go": "package cmd\nfunc usingJWT() {}\n",
		},
	}
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), m.changedFilesFn, m.fileAtRefFn)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Diff(context.Background(), "repo1", "/root", "refA", "refB")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !hasName(res.Added, "usingJWT") {
		t.Errorf("expected usingJWT in Added, got %+v", res.Added)
	}
	if containsReason(res.DegradedReasons, changedsymbols.DegradedReasonNonSymbolChangesOnly) {
		t.Errorf("must not emit non_symbol_changes_only when a symbol was added: %v",
			res.DegradedReasons)
	}
	if containsReason(res.DegradedReasons, changedsymbols.DegradedReasonBaselineRefNotIndexed) {
		t.Errorf("must not emit baseline_ref_not_indexed for a legitimately added file: %v",
			res.DegradedReasons)
	}
}

func containsReason(rs []string, want string) bool {
	return slices.Contains(rs, want)
}

func hasName(cs []changedsymbols.SymbolChange, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}
