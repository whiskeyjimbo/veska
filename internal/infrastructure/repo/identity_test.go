package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestModuleHostPathAnchor(t *testing.T) {
	cases := []struct {
		mod  string
		want bool
	}{
		{"github.com/whiskeyjimbo/veska", true}, // Go host/path
		{"example.com/x", true},                 // host/path, single sub-path
		{"@org/pkg", true},                      // scoped npm
		{"@org", false},                         // scope without package
		{"myapp", false},                        // bare name
		{"mod/sub", false},                      // first segment has no dot
		{"", false},
	}
	for _, c := range cases {
		anchor, ok := moduleHostPathAnchor(c.mod)
		if ok != c.want {
			t.Errorf("moduleHostPathAnchor(%q) ok = %v, want %v", c.mod, ok, c.want)
		}
		if ok && anchor != c.mod {
			t.Errorf("moduleHostPathAnchor(%q) anchor = %q, want it unchanged", c.mod, anchor)
		}
	}
}

// writeFile is a tiny test helper for seeding a manifest file into root.
func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitInitOrigin initialises a git work-tree at dir with an origin remote, or
// skips when git is unavailable. (Whitebox twin of repo_test.gitInitWithRemote.)
func gitInitOrigin(t *testing.T, dir, originURL string) {
	t.Helper()
	for _, a := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "remote", "add", "origin", originURL},
	} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", a, err, out)
		}
	}
}

func TestResolveIdentity_TierSelection(t *testing.T) {
	ctx := context.Background()

	t.Run("host/path module wins", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module github.com/org/repo\n\ngo 1.22\n")
		tier, anchor, id := ResolveIdentity(ctx, dir)
		if tier != TierModuleHostPath {
			t.Fatalf("tier = %q, want %q", tier, TierModuleHostPath)
		}
		if anchor != "github.com/org/repo" {
			t.Fatalf("anchor = %q", anchor)
		}
		if id != hashAnchor("github.com/org/repo") {
			t.Fatalf("id not derived from anchor")
		}
		if !tier.Converges() {
			t.Errorf("host/path tier must converge")
		}
	})

	t.Run("bare module ranks below origin URL but above abs-root", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module myapp\n")
		tier, anchor, _ := ResolveIdentity(ctx, dir)
		if tier != TierModuleBare {
			t.Fatalf("tier = %q, want %q", tier, TierModuleBare)
		}
		if anchor != "myapp" {
			t.Fatalf("anchor = %q", anchor)
		}
		if tier.Converges() {
			t.Errorf("bare module must not converge")
		}
	})

}

func TestResolveIdentity_OriginAndAbsRoot(t *testing.T) {
	ctx := context.Background()

	t.Run("origin URL when no module manifest", func(t *testing.T) {
		dir := t.TempDir()
		gitInitOrigin(t, dir, "git@github.com:org/repo.git")
		tier, _, _ := ResolveIdentity(ctx, dir)
		if tier != TierOriginURL {
			t.Fatalf("tier = %q, want %q", tier, TierOriginURL)
		}
	})

	t.Run("abs-root fallback reproduces legacy repoID", func(t *testing.T) {
		dir := t.TempDir() // no go.mod, no git remote
		tier, anchor, id := ResolveIdentity(ctx, dir)
		if tier != TierAbsRoot {
			t.Fatalf("tier = %q, want %q", tier, TierAbsRoot)
		}
		if anchor != dir {
			t.Fatalf("anchor = %q, want canonical root %q", anchor, dir)
		}
		if id != repoID(dir) {
			t.Fatalf("abs-root id must be byte-identical to legacy repoID(): no churn for local-only repos")
		}
	})
}

// TestResolveIdentity_Converges is the core ADR-S0017 contract: the SAME module
// path indexed at two DIFFERENT absolute roots yields the SAME repo_id.
func TestResolveIdentity_Converges(t *testing.T) {
	ctx := context.Background()
	mk := func() string {
		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module github.com/org/shared\n")
		return dir
	}
	aliceRoot, bobRoot := mk(), mk()
	if aliceRoot == bobRoot {
		t.Fatal("test roots must differ")
	}
	_, _, aliceID := ResolveIdentity(ctx, aliceRoot)
	_, _, bobID := ResolveIdentity(ctx, bobRoot)
	if aliceID != bobID {
		t.Fatalf("convergence broken: %s != %s", aliceID, bobID)
	}
}
