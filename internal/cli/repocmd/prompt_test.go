// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repocmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func openAliasPromptPools(t *testing.T) *sqlite.Pools {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func seedTrackedRepo(t *testing.T, pools *sqlite.Pools, id, path string) {
	t.Helper()
	if _, err := pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, kind) VALUES (?, ?, ?, 'tracked')`,
		id, path, int64(1),
	); err != nil {
		t.Fatal(err)
	}
}

func TestAliasPrompt_YesBindsSuggested(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-1", "/tmp/whatever")

	var out bytes.Buffer
	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-1", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	got, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "bar")
	if !ok || got != "id-1" {
		t.Errorf("LookupAlias(bar) = (%q,%v); want (id-1,true)", got, ok)
	}
}

func TestAliasPrompt_NoSkips(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-2", "/tmp/x")

	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("n\n"),
		Stdout: &bytes.Buffer{},
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-2", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "bar"); ok {
		t.Error("alias should NOT have been bound on n")
	}
}

func TestAliasPrompt_CustomNameAccepted(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-3", "/tmp/x")

	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("mycustom\n"),
		Stdout: &bytes.Buffer{},
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-3", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "mycustom")
	if !ok || got != "id-3" {
		t.Errorf("LookupAlias(mycustom) = (%q,%v); want (id-3,true)", got, ok)
	}
}

func TestAliasPrompt_NonTTYSkipsSilently(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-4", "/tmp/x")

	var out bytes.Buffer
	deps := PromptDeps{
		IsTTY:  func() bool { return false },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-4", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent non-TTY; got output: %s", out.String())
	}
	if _, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "bar"); ok {
		t.Error("non-TTY must not bind alias")
	}
}

func TestAliasPrompt_FallsBackOnCollision(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-5", "/tmp/x")
	seedTrackedRepo(t, pools, "id-other", "/tmp/other")
	// Pre-bind "bar" to a different repo so the primary collides.
	if err := repo.SetAlias(context.Background(), pools.Write, "bar", "id-other", false); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-5", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	// Should have offered "foo-bar" (owner-name fallback) and bound it.
	got, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "foo-bar")
	if !ok || got != "id-5" {
		t.Errorf("LookupAlias(foo-bar) = (%q,%v); want (id-5,true)", got, ok)
	}
	if !strings.Contains(out.String(), "foo-bar") {
		t.Errorf("expected prompt to offer foo-bar; got %s", out.String())
	}
}

func TestAliasPrompt_BothCollideSkips(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-6", "/tmp/x")
	seedTrackedRepo(t, pools, "id-other", "/tmp/other")
	for _, n := range []string{"bar", "foo-bar"} {
		if err := repo.SetAlias(context.Background(), pools.Write, n, "id-other", false); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-6", CanonicalURL: "https://github.com/foo/bar"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("both-collide should skip prompt; got output: %s", out.String())
	}
}

func TestAliasPrompt_PathFormUsesBasename(t *testing.T) {
	pools := openAliasPromptPools(t)
	seedTrackedRepo(t, pools, "id-7", "/home/user/src/myproj")

	deps := PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &bytes.Buffer{},
	}
	if err := RunAliasSuggestPrompt(context.Background(), pools.Write,
		AliasTarget{RepoID: "id-7", RootPath: "/home/user/src/myproj"}, deps,
	); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := repo.LookupAlias(context.Background(), pools.Write, "myproj")
	if !ok || got != "id-7" {
		t.Errorf("LookupAlias(myproj) = (%q,%v); want (id-7,true)", got, ok)
	}
}
