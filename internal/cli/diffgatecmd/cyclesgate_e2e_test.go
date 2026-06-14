package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

type cycleVerdict struct {
	Pass      bool `json:"pass"`
	NewCycles []struct {
		Members []struct {
			NodeID     string `json:"node_id"`
			SymbolPath string `json:"symbol_path"`
		} `json:"members"`
	} `json:"new_cycles"`
	Failures []string `json:"failures"`
}

func runCycles(t *testing.T, home, repoDir string) (cycleVerdict, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunCycles(context.Background(), CycleParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	var v cycleVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	return v, err
}

// symbolsOf flattens the verdict's cycle members to the set of symbol_paths.
func symbolsOf(v cycleVerdict) map[string]bool {
	out := map[string]bool{}
	for _, g := range v.NewCycles {
		for _, m := range g.Members {
			out[m.SymbolPath] = true
		}
	}
	return out
}

// AC1: a single-file change that closes a mutual-recursion cycle absent at base
// must FAIL and name both members. Base: A->B (acyclic). Candidate: B->A added.
func TestRunCycles_E2E_NewCycle_Fails(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc A() { B() }\nfunc B() {}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc A() { B() }\nfunc B() { A() }\n" // B now calls A -> A<->B
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runCycles(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("new cycle must FAIL (ErrGateFailed); got %v verdict=%+v", err, v)
	}
	if v.Pass {
		t.Fatalf("want FAIL; got %+v", v)
	}
	syms := symbolsOf(v)
	if !syms["A"] || !syms["B"] {
		t.Fatalf("cycle must name A and B; got %+v", v)
	}
}

// AC2: adding an acyclic CALLS edge must PASS. Base: A,B isolated. Candidate: A
// calls B (no back-edge).
func TestRunCycles_E2E_AcyclicAdd_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc A() {}\nfunc B() {}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc A() { B() }\nfunc B() {}\n"
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runCycles(t, home, repoDir)
	if err != nil {
		t.Fatalf("acyclic add must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("acyclic add must PASS; got %+v", v)
	}
}

// Pre-existing cycle, body-only change: modifying a member of a cycle that
// already existed at base must PASS (not net-new).
func TestRunCycles_E2E_PreExistingCycle_Passes(t *testing.T) {
	home := t.TempDir()
	const baseSrc = "package p\n\nfunc A() { B() }\nfunc B() { A() }\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": baseSrc})
	repoDir := t.TempDir()
	cand := "package p\n\nfunc A() { B(); _ = 1 }\nfunc B() { A() }\n"
	makeRepo(t, repoDir,
		map[string]string{"x.go": baseSrc},
		map[string]*string{"x.go": &cand},
	)

	v, err := runCycles(t, home, repoDir)
	if err != nil {
		t.Fatalf("pre-existing cycle must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("pre-existing cycle must PASS; got %+v", v)
	}
}

// Union-splice lock (false-NEGATIVE direction): a CROSS-FILE cycle closed by a
// change in ONE file. Base: a.go A->B; b.go B is a leaf. Candidate: only b.go
// changes so B->A. The completing edge B->A (src in changed b.go) is re-derived
// in the clone; the pre-existing A->B edge (src in UNCHANGED a.go, dst in
// changed b.go) is CASCADE-deleted by the re-promote and must be spliced back
// from base. Without the splice the clone shows only B->A -> no cycle -> false
// PASS. Non-substitutable by fakes: real cross-file promote + cascade.
func TestRunCycles_E2E_CrossFileCycle_Fails(t *testing.T) {
	home := t.TempDir()
	const aSrc = "package p\n\nfunc A() { B() }\n"
	const bSrc = "package p\n\nfunc B() {}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"a.go": aSrc, "b.go": bSrc})
	repoDir := t.TempDir()
	candB := "package p\n\nfunc B() { A() }\n" // only b.go changes; closes A<->B across files
	makeRepo(t, repoDir,
		map[string]string{"a.go": aSrc, "b.go": bSrc},
		map[string]*string{"b.go": &candB}, // a.go untouched
	)

	v, err := runCycles(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("cross-file cycle must FAIL (proves union splice); got %v verdict=%+v", err, v)
	}
	syms := symbolsOf(v)
	if !syms["A"] || !syms["B"] {
		t.Fatalf("cross-file cycle must name A and B; got %+v", v)
	}
}

// KNOWN LIMITATION (solov2-zvh6.11) — pins the documented index-ahead false-PASS
// so the behaviour is explicit and the hardening bead has a red target to flip.
// When the index has advanced ALL THE WAY to the candidate's content (here:
// seeded cyclic, which is also the candidate), both diff-gate legs collapse —
// base edges (from the live index) already hold the cycle, and ChangedNodeIDs
// goes empty because the overlay matches the index — so a net-new cycle PASSES.
// This is the out-of-contract index-ahead race (indexed-HEAD != base-ref) shared
// by the whole gate family, NOT the canonical contract the four tests above
// encode. Flip this to a FAIL assertion when zvh6.11 lands.
func TestRunCycles_E2E_IndexAhead_KnownLimitation(t *testing.T) {
	home := t.TempDir()
	const cyclic = "package p\n\nfunc A() { B() }\nfunc B() { A() }\n"
	const acyclic = "package p\n\nfunc A() { B() }\nfunc B() {}\n"
	// Index seeded AHEAD: at the candidate's cyclic content (violates the
	// indexed-HEAD == base-ref precondition documented on RunCycles).
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"x.go": cyclic})
	repoDir := t.TempDir()
	c := cyclic
	makeRepo(t, repoDir,
		map[string]string{"x.go": acyclic}, // base-ref HEAD~1 = acyclic
		map[string]*string{"x.go": &c},     // candidate HEAD = cyclic
	)

	v, err := runCycles(t, home, repoDir)
	if err != nil || !v.Pass {
		t.Fatalf("documented index-ahead limitation: gate PASSes today (zvh6.11 will flip this); got err=%v verdict=%+v", err, v)
	}
}
