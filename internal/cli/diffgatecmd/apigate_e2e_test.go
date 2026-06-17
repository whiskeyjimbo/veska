// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

type apiVerdict struct {
	Pass            bool `json:"pass"`
	BreakingChanges []struct {
		SymbolPath string `json:"symbol_path"`
		PrevSig    string `json:"prev_signature"`
		NewSig     string `json:"new_signature"`
	} `json:"breaking_changes"`
	RemovedSymbols []struct {
		SymbolPath string `json:"symbol_path"`
		FilePath   string `json:"file_path"`
		Kind       string `json:"kind"`
	} `json:"removed_symbols"`
	Failures []string `json:"failures"`
}

// runAPIRemoval seeds base with x.go=baseSrc, modifies it to candSrc, and runs
// the api gate - the shared harness for the removal/shape e2e cases.
func runAPIRemoval(t *testing.T, baseSrc, candSrc string) (apiVerdict, error) {
	t.Helper()
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	c := candSrc
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &c},
	)
	return runAPI(t, home, repoDir)
}

// removedSymbols flattens the verdict's removed-symbol set to names.
func removedSymbols(v apiVerdict) map[string]bool {
	out := map[string]bool{}
	for _, r := range v.RemovedSymbols {
		out[r.SymbolPath] = true
	}
	return out
}

func runAPI(t *testing.T, home, repoDir string) (apiVerdict, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunAPIBreak(context.Background(), APIParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	var v apiVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	return v, err
}

func apiSymbols(v apiVerdict) map[string]bool {
	out := map[string]bool{}
	for _, c := range v.BreakingChanges {
		out[c.SymbolPath] = true
	}
	return out
}

// AC1: changing an EXPORTED symbol's signature shape (arity change, so the
// signature string unambiguously differs) must FAIL and name the symbol.
func TestRunAPIBreak_E2E_ExportedSignatureChange_Fails(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo(a int) int { return a }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc Foo(a int, b int) int { return a + b }\n" // arity change
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("exported signature change must FAIL (ErrGateFailed); got %v verdict=%+v", err, v)
	}
	if !apiSymbols(v)["Foo"] {
		t.Fatalf("must name Foo; got %+v", v)
	}
}

// AC2: changing only an UNEXPORTED symbol's signature must PASS.
func TestRunAPIBreak_E2E_UnexportedSignatureChange_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc foo(a int) int { return a }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc foo(a int, b int) int { return a + b }\n"
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if err != nil {
		t.Fatalf("unexported signature change must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("unexported signature change must PASS; got %+v", v)
	}
}

// AC3: a body-only change to an EXPORTED symbol (signature unchanged) must PASS.
func TestRunAPIBreak_E2E_ExportedBodyOnly_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo(a int) int { return a }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc Foo(a int) int { return a * 2 }\n" // body only; same signature
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if err != nil {
		t.Fatalf("exported body-only change must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("exported body-only change must PASS; got %+v", v)
	}
}

// Added-exported-function PASS: adding a brand-new exported function (nothing
// else changes) is NOT a breaking change. Under base-ref pinning the added file
// is deleted from the base clone, so the new node is created fresh on the
// after-clone with no prior signature - the drift query must NOT treat a
// null/empty prev_signature as drift, or every added export would false-FAIL.
func TestRunAPIBreak_E2E_AddedExportedFunc_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo(a int) int { return a }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	barSrc := "package p\n\nfunc Bar(a int) int { return a }\n" // new exported func, new file
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"bar.go": &barSrc},
	)

	v, err := runAPI(t, home, repoDir)
	if err != nil {
		t.Fatalf("added exported func must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("added exported func is not breaking; must PASS; got %+v", v)
	}
}

// INDEX-AHEAD HARDENING - the former false-PASS, now FAILing.
// The index is seeded AHEAD at the candidate's changed content. Before pinning,
// each node's prev_signature equalled its signature after the re-promote (the
// clone-of-the-live-index already held the candidate sig), so contract-drift
// never fired and a breaking exported-signature change wrongly PASSED. With
// buildPinnedEphemeral the after-state is cloned from a base-ref-pinned clone,
// so prev_signature is the BASE-REF signature and drift correctly fires. The
// prev_signature VALUE assertion is the sharp probe: it must be the single-arg
// base-ref form, NOT the two-arg candidate form a drifted clone would carry.
func TestRunAPIBreak_E2E_IndexAhead_NowDetected(t *testing.T) {
	home := t.TempDir()
	const changed = "package p\n\nfunc Foo(a int, b int) int { return a + b }\n"
	const base = "package p\n\nfunc Foo(a int) int { return a }\n"
	// Index seeded AHEAD: at the candidate's changed content.
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": changed})
	repoDir := t.TempDir()
	c := changed
	makeRepo(t, repoDir,
		map[string]string{"x.go": base}, // base-ref HEAD~1
		map[string]*string{"x.go": &c},  // candidate HEAD
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("index-ahead breaking change must now FAIL (zvh6.11); got err=%v verdict=%+v", err, v)
	}
	if len(v.BreakingChanges) != 1 {
		t.Fatalf("want exactly one breaking change (Foo); got %+v", v)
	}
	bc := v.BreakingChanges[0]
	if bc.PrevSig != "Foo(a int) int" {
		t.Fatalf("prev_signature must be the BASE-REF sig (not the drifted index's candidate sig); got %q", bc.PrevSig)
	}
	if bc.NewSig != "Foo(a int, b int) int" {
		t.Fatalf("new_signature must be the candidate sig; got %q", bc.NewSig)
	}
}

// Removal: deleting an EXPORTED function must FAIL, naming it.
func TestRunAPIBreak_E2E_RemovedExportedFunc_Fails(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc Foo() int { return 1 }\n" // Bar removed
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("removing exported Bar must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Bar"] {
		t.Fatalf("must name Bar removed; got %+v", v)
	}
}

// Rename: renaming an EXPORTED function FAILs - the OLD name is
// gone (breaking for importers). Removal/rename collapse into one category.
func TestRunAPIBreak_E2E_RenamedExportedFunc_Fails(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo() int { return 1 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc Renamed() int { return 1 }\n" // Foo -> Renamed
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("renaming exported Foo must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Foo"] {
		t.Fatalf("must name the absent OLD symbol Foo; got %+v", v)
	}
}

// Unexport-in-place: lowercasing an EXPORTED symbol FAILs
// the exported name disappears from the public surface. This pins that the
// candidate-side exported filter is what makes unexporting register as a
// removal (base-exported {Foo}, candidate exports nothing → Foo absent).
func TestRunAPIBreak_E2E_UnexportInPlace_Fails(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo() int { return 1 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc foo() int { return 1 }\n" // Foo -> foo (unexported)
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("unexporting Foo must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Foo"] {
		t.Fatalf("must name the now-absent exported Foo; got %+v", v)
	}
}

// Unexporting-as-removal negative: removing an UNEXPORTED function is not a
// public-API break - it never entered the base-exported set, so PASS.
func TestRunAPIBreak_E2E_RemovedUnexportedFunc_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc Foo() int { return 1 }\nfunc helper() int { return 2 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc Foo() int { return 1 }\n" // unexported helper removed
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runAPI(t, home, repoDir)
	if err != nil {
		t.Fatalf("removing unexported helper must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("removing unexported helper must PASS; got %+v", v)
	}
}

// Deleted file: deleting a file that defined an EXPORTED symbol
// must FAIL. Exercises buildPinnedEphemeral's deleted-file→base-ref-content path
// for the base-exported leg.
func TestRunAPIBreak_E2E_DeletedFileRemovesExport_Fails(t *testing.T) {
	home := t.TempDir()
	const aSrc = "package p\n\nfunc Foo() int { return 1 }\n"
	const bSrc = "package p\n\nfunc Bar() int { return 2 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"a.go": aSrc, "b.go": bSrc})
	repoDir := t.TempDir()
	makeRepo(t, repoDir,
		map[string]string{"a.go": aSrc, "b.go": bSrc},
		map[string]*string{"b.go": nil}, // delete b.go (and its exported Bar)
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("deleting a file with an exported symbol must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Bar"] {
		t.Fatalf("must name Bar removed; got %+v", v)
	}
}

// Removal of an exported TYPE/STRUCT must FAIL.
func TestRunAPIBreak_E2E_RemovedExportedType_Fails(t *testing.T) {
	v, err := runAPIRemoval(t,
		"package p\n\ntype Config struct{ A int }\n\nfunc Keep() {}\n",
		"package p\n\nfunc Keep() {}\n", // Config removed
	)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("removing exported type Config must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Config"] {
		t.Fatalf("must name Config removed; got %+v", v)
	}
}

// Removal of an exported grouped CONST must FAIL. Go const +
// var both surface as the parser's KindVariable; this pins that the grouped
// const_declaration path produces a removable exported node.
func TestRunAPIBreak_E2E_RemovedExportedConst_Fails(t *testing.T) {
	v, err := runAPIRemoval(t,
		"package p\n\nconst (\n\tMaxN = 10\n\tMinN = 1\n)\n\nfunc Keep() {}\n",
		"package p\n\nconst (\n\tMinN = 1\n)\n\nfunc Keep() {}\n", // MaxN removed
	)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("removing exported const MaxN must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["MaxN"] {
		t.Fatalf("must name MaxN removed; got %+v", v)
	}
}

// Removal of an exported VAR must FAIL.
func TestRunAPIBreak_E2E_RemovedExportedVar_Fails(t *testing.T) {
	v, err := runAPIRemoval(t,
		"package p\n\nvar Default = 7\n\nfunc Keep() {}\n",
		"package p\n\nfunc Keep() {}\n", // Default removed
	)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("removing exported var Default must FAIL; got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Default"] {
		t.Fatalf("must name Default removed; got %+v", v)
	}
}

// A same-name type SHAPE change (struct -> interface) is NOT a removal: the
// exported NAME persists (, kind dropped from identity). It must
// PASS the removal detector. (Type shape drift is a separate, currently
// undetected concern - neither removal nor signature-drift covers it.)
func TestRunAPIBreak_E2E_TypeShapeChange_NotRemoval_Passes(t *testing.T) {
	v, err := runAPIRemoval(t,
		"package p\n\ntype Config struct{ A int }\n\nfunc Keep() {}\n",
		"package p\n\ntype Config interface{ A() int }\n\nfunc Keep() {}\n", // struct -> interface, same name
	)
	if err != nil {
		t.Fatalf("same-name type shape change must PASS the removal detector; got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("struct->interface (name persists) must NOT be reported as removal; got %+v", v)
	}
}

// The discriminating e2e (proves package-scoped identity, not node_id): moving
// an EXPORTED function to another file in the SAME package (root dir) must PASS.
func TestRunAPIBreak_E2E_IntraPackageMove_Passes(t *testing.T) {
	home := t.TempDir()
	const aSrc = "package p\n\nfunc Foo() int { return 1 }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"a.go": aSrc})
	repoDir := t.TempDir()
	emptied := "package p\n"                              // Foo removed from a.go
	moved := "package p\n\nfunc Foo() int { return 1 }\n" // Foo now in b.go
	makeRepo(t, repoDir,
		map[string]string{"a.go": aSrc},
		map[string]*string{"a.go": &emptied, "b.go": &moved},
	)

	v, err := runAPI(t, home, repoDir)
	if err != nil {
		t.Fatalf("intra-package move must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("intra-package move must PASS (identity = pkg+kind+name); got %+v", v)
	}
}

// INDEX-AHEAD removal lock ( +.12): the index is seeded AHEAD
// it already reflects Bar's removal - while base-ref still has Bar. Before
// pinning, the base-exported leg (read from a clone of the drifted index) would
// also lack Bar, so no removal is detected → false PASS. With buildPinnedEphemeral
// the base clone re-promotes base-ref's x.go (Bar present), so the removal is
// correctly detected and the gate FAILs.
func TestRunAPIBreak_E2E_IndexAhead_Removal_NowDetected(t *testing.T) {
	home := t.TempDir()
	const withBar = "package p\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n"
	const withoutBar = "package p\n\nfunc Foo() int { return 1 }\n"
	// Index seeded AHEAD: at the candidate content (Bar already gone).
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": withoutBar})
	repoDir := t.TempDir()
	c := withoutBar
	makeRepo(t, repoDir,
		map[string]string{"x.go": withBar}, // base-ref HEAD~1 still has Bar
		map[string]*string{"x.go": &c},     // candidate HEAD removes Bar
	)

	v, err := runAPI(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("index-ahead removal must now FAIL (zvh6.11/.12); got %v verdict=%+v", err, v)
	}
	if !removedSymbols(v)["Bar"] {
		t.Fatalf("must name Bar removed; got %+v", v)
	}
}
