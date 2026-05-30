package searchcmd

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func openPromptPools(t *testing.T) *sqlite.Pools {
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

func seedEphemeral(t *testing.T, pools *sqlite.Pools, id, path string) repo.Record {
	t.Helper()
	if _, err := pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, kind, canonical_url) VALUES (?, ?, ?, 'ephemeral', ?)`,
		id, path, int64(1), "https://example.com/foo/bar",
	); err != nil {
		t.Fatal(err)
	}
	return repo.Record{RepoID: id, RootPath: path, Kind: "ephemeral"}
}

func TestRunAcceptancePrompt_YesPromotes(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-1", "/tmp/eph-1")

	var out bytes.Buffer
	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	// AC1: y → kind='tracked' + prompted_at set.
	var kind string
	var promptedAt sql.NullInt64
	if err := pools.ReadDB.QueryRow(`SELECT kind, prompted_at FROM repos WHERE repo_id = ?`, "eph-1").Scan(&kind, &promptedAt); err != nil {
		t.Fatal(err)
	}
	if kind != "tracked" {
		t.Errorf("kind = %q, want tracked", kind)
	}
	if !promptedAt.Valid || promptedAt.Int64 == 0 {
		t.Errorf("prompted_at = %v, want set", promptedAt)
	}
	if !strings.Contains(out.String(), "promoted") {
		t.Errorf("expected promotion confirmation in output: %s", out.String())
	}
}

func TestRunAcceptancePrompt_NoKeepsEphemeral(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-2", "/tmp/eph-2")

	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("n\n"),
		Stdout: &bytes.Buffer{},
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var kind string
	var promptedAt sql.NullInt64
	if err := pools.ReadDB.QueryRow(`SELECT kind, prompted_at FROM repos WHERE repo_id = ?`, "eph-2").Scan(&kind, &promptedAt); err != nil {
		t.Fatal(err)
	}
	if kind != "ephemeral" {
		t.Errorf("kind = %q, want ephemeral", kind)
	}
	if !promptedAt.Valid {
		t.Error("prompted_at must be set even after 'n' so we don't re-prompt")
	}
}

func TestRunAcceptancePrompt_EmptyAnswerTreatedAsNo(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-3", "/tmp/eph-3")

	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("\n"),
		Stdout: &bytes.Buffer{},
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var kind string
	if err := pools.ReadDB.QueryRow(`SELECT kind FROM repos WHERE repo_id = ?`, "eph-3").Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "ephemeral" {
		t.Errorf("blank answer should keep ephemeral; got %q", kind)
	}
}

func TestRunAcceptancePrompt_NonTTYPrintsHintNoWrite(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-4", "/tmp/eph-4")

	var out bytes.Buffer
	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { return false }, // pipe / script / MCP
		Stdin:  strings.NewReader(""),
		Stdout: &out,
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	// AC2: hint printed; prompted_at NOT set (so a later TTY run can prompt).
	if !strings.Contains(out.String(), "veska repo add https://example.com/foo/bar") {
		t.Errorf("expected hint with `veska repo add <url>`; got: %s", out.String())
	}
	var promptedAt sql.NullInt64
	if err := pools.ReadDB.QueryRow(`SELECT prompted_at FROM repos WHERE repo_id = ?`, "eph-4").Scan(&promptedAt); err != nil {
		t.Fatal(err)
	}
	if promptedAt.Valid {
		t.Errorf("non-TTY must NOT set prompted_at; got %v", promptedAt)
	}
}

func TestRunAcceptancePrompt_AlreadyPromptedIsNoOp(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-5", "/tmp/eph-5")
	if _, err := pools.Write.Exec(`UPDATE repos SET prompted_at = ? WHERE repo_id = ?`, int64(42), "eph-5"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	called := false
	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { called = true; return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &out,
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if called {
		t.Error("AC3: prompted_at already set must short-circuit before isatty check")
	}
	if out.Len() != 0 {
		t.Errorf("expected silent no-op; got output: %s", out.String())
	}

	var kind string
	if err := pools.ReadDB.QueryRow(`SELECT kind FROM repos WHERE repo_id = ?`, "eph-5").Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "ephemeral" {
		t.Errorf("already-prompted row must not be modified; kind=%q", kind)
	}
}

func TestRunAcceptancePrompt_TrackedRowIsNoOp(t *testing.T) {
	pools := openPromptPools(t)
	if _, err := pools.Write.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, kind) VALUES (?, ?, ?, 'tracked')`,
		"trk", "/tmp/trk", int64(1),
	); err != nil {
		t.Fatal(err)
	}
	rec := repo.Record{RepoID: "trk", Kind: "tracked"}

	var out bytes.Buffer
	deps := repocmd.PromptDeps{
		IsTTY: func() bool {
			t.Error("tracked row must not trigger isatty check")
			return true
		},
		Stdin:  strings.NewReader(""),
		Stdout: &out,
	}
	if err := RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/x", deps); err != nil {
		t.Fatalf("prompt: %v", err)
	}
}

func TestRunAcceptancePrompt_PromotionIsInPlace(t *testing.T) {
	pools := openPromptPools(t)
	rec := seedEphemeral(t, pools, "eph-place", "/tmp/eph-place")

	// Capture the row path before promotion.
	var beforePath string
	_ = pools.ReadDB.QueryRow(`SELECT root_path FROM repos WHERE repo_id = ?`, "eph-place").Scan(&beforePath)

	deps := repocmd.PromptDeps{
		IsTTY:  func() bool { return true },
		Stdin:  strings.NewReader("y\n"),
		Stdout: &bytes.Buffer{},
	}
	_ = RunAcceptancePrompt(context.Background(), pools.Write, rec, "https://example.com/foo/bar", deps)

	// AC4: same repo_id, same root_path, no row replacement.
	var afterID, afterPath, afterKind string
	if err := pools.ReadDB.QueryRow(`SELECT repo_id, root_path, kind FROM repos WHERE repo_id = ?`, "eph-place").Scan(&afterID, &afterPath, &afterKind); err != nil {
		t.Fatal(err)
	}
	if afterID != "eph-place" || afterPath != beforePath {
		t.Errorf("row identity drifted: id=%q path=%q", afterID, afterPath)
	}
	if afterKind != "tracked" {
		t.Errorf("kind = %q, want tracked", afterKind)
	}
}
