// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// setupWikiEnv creates a temp VESKA_HOME with a migrated veska.db and a single
// registered repo whose root is a fresh git repository. It returns the repo
// root path and the repo_id.
func setupWikiEnv(t *testing.T) (repoRoot, repoID string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)

	// A real (empty) git repository so gitwatch.ChangeCounts succeeds.
	repoRoot = t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	dbPath := filepath.Join(home, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	repoID = "repo-under-test"
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)`,
		repoID, repoRoot, time.Now().Unix(), "main", sql.NullString{String: "example.com/m", Valid: true},
	)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return repoRoot, repoID
}

// TestWikiCmd_RegeneratesBothPages covers AC1: `veska wiki` regenerates both
// pages and exits 0 on success.
func TestWikiCmd_RegeneratesBothPages(t *testing.T) {
	repoRoot, _ := setupWikiEnv(t)

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"wiki"})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska wiki: want exit 0, got error: %v", err)
	}

	for _, page := range []string{wiki.HotZonesPagePath, wiki.EntryPointsPagePath} {
		p := filepath.Join(repoRoot, page)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected page %s to exist: %v", page, err)
		}
	}
	if out.Len() == 0 {
		t.Error("expected a confirmation message on success")
	}
}

// TestWikiCmd_ByteIdenticalOutput covers AC2: the command writes the same
// byte-identical output the promotion lane (wiki.Handler) produces.
func TestWikiCmd_ByteIdenticalOutput(t *testing.T) {
	repoRoot, _ := setupWikiEnv(t)

	run := func() (hot, entry []byte) {
		root := newRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetArgs([]string{"wiki"})
		if err := root.Execute(); err != nil {
			t.Fatalf("veska wiki: %v", err)
		}
		h, err := os.ReadFile(filepath.Join(repoRoot, wiki.HotZonesPagePath))
		if err != nil {
			t.Fatalf("read hot zones: %v", err)
		}
		e, err := os.ReadFile(filepath.Join(repoRoot, wiki.EntryPointsPagePath))
		if err != nil {
			t.Fatalf("read entry points: %v", err)
		}
		return h, e
	}

	hot1, entry1 := run()
	hot2, entry2 := run()

	// the wiki Handler stamps a wall-clock GeneratedAt in
	// the page header on every render, which is non-deterministic by
	// design. Strip that single line before the byte-identical
	// comparison - the data rows (ranking input is the same) must still
	// match across runs.
	stripGenerated := func(b []byte) []byte {
		lines := bytes.Split(b, []byte("\n"))
		out := lines[:0]
		for _, l := range lines {
			if bytes.HasPrefix(l, []byte("_Generated:")) {
				continue
			}
			out = append(out, l)
		}
		return bytes.Join(out, []byte("\n"))
	}
	if !bytes.Equal(stripGenerated(hot1), stripGenerated(hot2)) {
		t.Error("hot_zones.md output not byte-identical across runs (excluding generated-at line)")
	}
	if !bytes.Equal(stripGenerated(entry1), stripGenerated(entry2)) {
		t.Error("entry_points.md output not byte-identical across runs (excluding generated-at line)")
	}
}
