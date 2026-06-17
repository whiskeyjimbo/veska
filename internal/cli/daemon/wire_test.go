// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// testConfig returns a Config whose paths live under a fresh tmp dir so each
// test is hermetic (no ~/.veska state shared across runs).
func testConfig(t *testing.T) Config {
	t.Helper()
	home := t.TempDir()
	return Config{
		VeskaHome:     home,
		SQLitePath:    filepath.Join(home, "veska.db"),
		CLISockPath:   filepath.Join(home, "cli.sock"),
		MCPSockPath:   filepath.Join(home, "mcp.sock"),
		VectorBackend: vector.BackendMemory,
		OllamaURL:     "http://127.0.0.1:0", // unreachable; not dialed at construct time
		EmbedModel:    "nomic-embed-text",
	}
}

// TestWire_Constructs verifies newDaemon assembles the full collaborator
// graph from a minimal but valid config without error or panic.
func TestWire_Constructs(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("newDaemon returned nil daemon")
	}
	t.Cleanup(func() { _ = d.Stop() })
}

// TestDaemon_ReconcilerWired_FieldPresent is the composition-root smoke test for
// the wake-reconciler wiring: newDaemon must populate d.reconciler so Start can
// spawn the suspend/resume tick loop. A nil field means the wiring was dropped.
// The gap-detection behaviour itself is covered by the git package's
// TestStart_WakeGapTriggersSweep.
func TestDaemon_ReconcilerWired_FieldPresent(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if d.reconciler == nil {
		t.Fatal("d.reconciler is nil after newDaemon; wiring missing")
	}

	// Start then Stop must cleanly spin up and tear down the reconciler
	// goroutine (recDone) without wedging shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestWire_UnknownVectorBackend ensures an invalid VESKA_VECTOR_BACKEND value
// surfaces as a typed *ErrMissingDep rather than as a generic open error.
func TestWire_UnknownVectorBackend(t *testing.T) {
	cfg := testConfig(t)
	cfg.VectorBackend = "not-a-real-backend"
	_, err := newDaemon(cfg)
	if err == nil {
		t.Fatal("expected error for unknown vector backend")
	}
	var miss *ErrMissingDep
	if !errors.As(err, &miss) {
		t.Fatalf("want *ErrMissingDep, got %T: %v", err, err)
	}
	if miss.Name != "vector_backend" {
		t.Fatalf("missing dep name = %q; want %q", miss.Name, "vector_backend")
	}
}

// TestWire_HonorsEnvVectorBackend ensures the VESKA_VECTOR_BACKEND env var is
// resolved when Config.VectorBackend is empty.
func TestWire_HonorsEnvVectorBackend(t *testing.T) {
	t.Setenv("VESKA_VECTOR_BACKEND", string(vector.BackendMemory))
	cfg := testConfig(t)
	cfg.VectorBackend = "" // force env path
	resolved := ResolveConfig(cfg)
	if resolved.VectorBackend != vector.BackendMemory {
		t.Fatalf("VectorBackend = %q; want %q", resolved.VectorBackend, vector.BackendMemory)
	}
}

// TestErrMissingDep_Format formats both with and without the Why field so
// operators see a useful message in either path.
func TestErrMissingDep_Format(t *testing.T) {
	got := (&ErrMissingDep{Name: "x"}).Error()
	if got == "" {
		t.Fatal("empty error string")
	}
	got = (&ErrMissingDep{Name: "x", Why: "because"}).Error()
	if got == "" || !contains(got, "because") {
		t.Fatalf("Error() = %q; want it to contain Why", got)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestWire_StartStop verifies Start creates the sockets and Stop removes them.
// Both are exercised under a short bounded timeout to catch goroutine leaks.
func TestWire_StartStop(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// CLI + MCP sockets should be present.
	for _, p := range []string{cfg.CLISockPath, cfg.MCPSockPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected socket %q to exist after Start: %v", p, err)
		}
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Sockets should be cleaned up.
	for _, p := range []string{cfg.CLISockPath, cfg.MCPSockPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected socket %q to be removed after Stop; stat err=%v", p, err)
		}
	}
}

// TestWire_StartStopIdempotent verifies calling Start / Stop more than once
// does not deadlock or panic.
func TestWire_StartStopIdempotent(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestResolveConfig_AppliesDefaults confirms that an empty Config picks up
// every env-or-fallback default so newDaemon never sees a zero field.
func TestResolveConfig_AppliesDefaults(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	t.Setenv("VESKA_VECTOR_BACKEND", "")
	t.Setenv("VESKA_OLLAMA_URL", "")
	t.Setenv("VESKA_EMBED_MODEL", "")

	got := ResolveConfig(Config{})
	if got.VeskaHome == "" {
		t.Error("VeskaHome empty after resolve")
	}
	if got.SQLitePath == "" {
		t.Error("SQLitePath empty after resolve")
	}
	if got.CLISockPath == "" {
		t.Error("CLISockPath empty after resolve")
	}
	if got.MCPSockPath == "" {
		t.Error("MCPSockPath empty after resolve")
	}
	if got.VectorBackend != vector.BackendMemory {
		t.Errorf("VectorBackend = %q; want %q", got.VectorBackend, vector.BackendMemory)
	}
	// EmbedModel is intentionally NOT defaulted daemon-wide anymore: it
	// only matters when the elected embedder is Ollama, and elect supplies
	// the nomic default there. Unset env ⇒ empty here.
	if got.EmbedModel != "" {
		t.Errorf("EmbedModel = %q; want empty (no daemon-wide default)", got.EmbedModel)
	}
	if got.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q; want default", got.OllamaURL)
	}
}

// TestWire_RegistersAdminTools verifies registerMCPTools wires the 5 admin
// MCP tools so they resolve instead of surfacing as MethodNotFound.
func TestWire_RegistersAdminTools(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	want := []string{
		"eng_get_current_repo",
		"eng_list_repos",
		"eng_get_repo",
		"eng_get_status",
		"eng_get_config",
	}
	have := make(map[string]bool)
	for _, n := range d.mcpRegistry().Names() {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("admin tool %q not registered; have=%v", n, d.mcpRegistry().Names())
		}
	}
}

// TestWire_RegistersGraphBlastSearchTools verifies the graph, blast-radius,
// and semantic-search MCP tool families are wired so they resolve instead of
// surfacing as MethodNotFound.
func TestWire_RegistersGraphBlastSearchTools(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	have := make(map[string]bool)
	for _, n := range d.mcpRegistry().Names() {
		have[n] = true
	}
	want := []string{
		// graph
		"eng_find_symbol", "eng_get_node", "eng_get_call_chain", "eng_get_file_nodes",
		// blast
		"eng_get_blast_radius", "eng_get_dirty_blast_radius", "eng_get_diff_blast_radius",
		// search
		"eng_search_semantic", "eng_search_similar",
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("tool %q not registered; have=%v", n, d.mcpRegistry().Names())
		}
	}
}

// TestWire_RegistersFinalFiveTools verifies the record/repo tools
// (eng_get_finding, eng_get_suppression, eng_close_suppression, eng_add_repo,
// eng_remove_repo) resolve, and that the full registered surface is 33 tools
// (32 + eng_find_changed_symbols, added by ).
func TestWire_RegistersFinalFiveTools(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	names := d.mcpRegistry().Names()
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, n := range []string{
		"eng_get_finding", "eng_get_suppression", "eng_close_suppression",
		"eng_add_repo", "eng_remove_repo", "eng_find_changed_symbols",
		"eng_promote_repo", // post-commit hook target
		"eng_reindex_repo", // in-daemon reindex dispatch
	} {
		if !have[n] {
			t.Errorf("tool %q not registered; have=%v", n, names)
		}
	}

	// the task tools (eng_set_active_task / get_active_task /
	// get_task_history) are parked until a backend exists. 34 → 31.
	// adds eng_reindex_repo: 31 → 32.
	// adds eng_list_dependencies: 32 → 33.
	// adds eng_find_related: 33 → 34.
	// adds eng_set_repo_alias + eng_remove_repo_alias: 34 → 36.
	// adds eng_find_clones: 36 → 37.
	// adds eng_find_clusters: 37 → 38.
	if got := len(names); got != 38 {
		t.Errorf("registered tool count = %d; want 38; have=%v", got, names)
	}
	// Negative-check: parked tools must NOT appear.
	for _, parked := range []string{
		"eng_set_active_task", "eng_get_active_task", "eng_get_task_history",
	} {
		if have[parked] {
			t.Errorf("parked tool %q unexpectedly registered ", parked)
		}
	}
}

// TestWire_StartWatchesRegisteredRepos verifies that a repo registered in the
// daemon's SQLite repos table before Start is added to the fsnotify watcher.
func TestWire_StartWatchesRegisteredRepos(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	// Register a repo before Start.
	gitDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitDir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	repoID, _, err := repo.Add(context.Background(), d.pools.Write, gitDir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	watched := d.watcher.WatchedRepoIDs()
	found := false
	for _, id := range watched {
		if id == repoID {
			found = true
		}
	}
	if !found {
		t.Errorf("watcher does not watch registered repo %q; watched=%v", repoID, watched)
	}
}

// TestWire_WatchLoopRoutesEditsToStaging exercises the end-to-end fsnotify
// chain we cared about in: file write in a watched repo →
// MultiRepoWatcher → runWatchLoop → Ingester.Save → StagingArea contains the
// file under the repo's active_branch. Parameterised over branch to make sure
// the hardcoded "main" regression doesn't reappear.
func TestWire_WatchLoopRoutesEditsToStaging(t *testing.T) {
	for _, branch := range []string{"main", "trunk"} {
		t.Run(branch, func(t *testing.T) {
			cfg := testConfig(t)
			d, err := newDaemon(cfg)
			if err != nil {
				t.Fatalf("newDaemon: %v", err)
			}
			t.Cleanup(func() { _ = d.Stop() })

			gitDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(gitDir, ".git", "hooks"), 0o755); err != nil {
				t.Fatalf("create .git/hooks: %v", err)
			}
			repoID, _, err := repo.Add(context.Background(), d.pools.Write, gitDir)
			if err != nil {
				t.Fatalf("repo.Add: %v", err)
			}
			// repo.Add defaults to "main" when HEAD detection fails (this
			// fixture has no real git init). Force the recorded branch so
			// runWatchLoop's lookup must honour something other than "main".
			if _, err := d.pools.Write.Exec(
				`UPDATE repos SET active_branch = ? WHERE repo_id = ?`,
				branch, repoID,
			); err != nil {
				t.Fatalf("force active_branch: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}

			src := "package g\n\nfunc Hello() string { return \"hi\" }\n"
			abs := filepath.Join(gitDir, "hello.go")
			if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
				t.Fatalf("write hello.go: %v", err)
			}

			// the watch loop relativises the absolute fsnotify
			// path before staging, so the staged key is the repo-relative form.
			const wantRel = "hello.go"
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				files := d.staging.StagedFiles(repoID, branch)
				if slices.Contains(files, wantRel) {
					return // success - staged under correct branch
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Errorf("staging[branch=%q] did not receive hello.go within 3s; main=%v empty=%v %s=%v",
				branch,
				d.staging.StagedFiles(repoID, "main"),
				d.staging.StagedFiles(repoID, ""),
				branch, d.staging.StagedFiles(repoID, branch))
		})
	}
}
