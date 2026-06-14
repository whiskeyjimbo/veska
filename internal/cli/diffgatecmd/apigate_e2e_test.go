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
	Failures []string `json:"failures"`
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
// after-clone with no prior signature — the drift query must NOT treat a
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

// INDEX-AHEAD HARDENING (solov2-zvh6.11) — the former false-PASS, now FAILing.
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
