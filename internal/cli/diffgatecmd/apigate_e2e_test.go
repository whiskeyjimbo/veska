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

// KNOWN LIMITATION (solov2-zvh6.11) — pins the documented index-ahead false-PASS.
// When the index already holds the candidate's changed-file content, each node's
// prev_signature equals its signature after the re-promote, so contract-drift
// never fires and a breaking exported-signature change PASSES. This is the
// out-of-contract index-ahead race (indexed-HEAD != base-ref) shared by the gate
// family, NOT the canonical contract the three AC tests above encode. Flip this
// to a FAIL assertion when zvh6.11 lands.
func TestRunAPIBreak_E2E_IndexAhead_KnownLimitation(t *testing.T) {
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
	if err != nil || !v.Pass {
		t.Fatalf("documented index-ahead limitation: gate PASSes today (zvh6.11 will flip this); got err=%v verdict=%+v", err, v)
	}
}
