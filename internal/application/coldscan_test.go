// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	infrafs "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
)

// realIgnoreLoader adapts infrastructure/fs.Load to the application-layer
// IgnoreLoader contract for use in cold-scan tests. Test files may freely
// import infrastructure; layercheck only inspects non-test compilation
// units.
func realIgnoreLoader(repoRoot string) (IgnoreMatcher, error) {
	return infrafs.Load(repoRoot)
}

// fakeGitQuerier supplies a deterministic HEAD for cold-scan tests.
type fakeGitQuerier struct {
	head    string
	headErr error
}

func (f *fakeGitQuerier) HEAD(string) (string, error) { return f.head, f.headErr }
func (f *fakeGitQuerier) IsAncestor(string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeGitQuerier) CommitsSince(string, string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeGitQuerier) ChangedFiles(string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeGitQuerier) ReadFileAtCommit(string, string, string) ([]byte, error) {
	return nil, nil
}

type recordedSave struct {
	RepoID string
	Branch string
	Path   string
	Src    []byte
}

type recordedPromote struct {
	RepoID string
	Branch string
	SHA    string
	Actor  domain.Actor
}

// captureFakes provides thread-safe capturing saveFn / promoteFn closures.
type captureFakes struct {
	mu         sync.Mutex
	saves      []recordedSave
	promotes   []recordedPromote
	saveHook   func(ctx context.Context, repoID, branch, path string, src []byte)
	promoteErr error
}

func (c *captureFakes) save(ctx context.Context, repoID, branch, path string, src []byte) {
	c.mu.Lock()
	c.saves = append(c.saves, recordedSave{repoID, branch, path, append([]byte(nil), src...)})
	hook := c.saveHook
	c.mu.Unlock()
	if hook != nil {
		hook(ctx, repoID, branch, path, src)
	}
}

func (c *captureFakes) promote(_ context.Context, repoID, branch, sha string, actor domain.Actor) error {
	c.mu.Lock()
	c.promotes = append(c.promotes, recordedPromote{repoID, branch, sha, actor})
	err := c.promoteErr
	c.mu.Unlock()
	return err
}

func (c *captureFakes) savedPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths := make([]string, len(c.saves))
	for i, s := range c.saves {
		paths[i] = s.Path
	}
	sort.Strings(paths)
	return paths
}

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

func newReparser(t *testing.T, c *captureFakes, head string) func(context.Context, RepoRecord) error {
	t.Helper()
	r, err := newColdScanReparserFromFns(c.save, c.promote, &fakeGitQuerier{head: head}, []string{".go", ".ts", ".tsx"}, WithIgnoreLoader(realIgnoreLoader))
	if err != nil {
		t.Fatalf("newColdScanReparserFromFns: %v", err)
	}
	return r
}

// TestColdScanReparser_TrackerSeesScanInFlight covers: when
// the reparser is built with WithScanTracker, the tracker reports the
// repo as in-flight while save is running, and empty after the
// reparser returns. We hook the saveHook to take a Snapshot mid-scan.
func TestColdScanReparser_TrackerSeesScanInFlight(t *testing.T) {
	tracker := NewScanTracker()
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")

	c := &captureFakes{}
	midSnapshot := make([]ScanState, 0)
	c.saveHook = func(_ context.Context, _, _, _ string, _ []byte) {
		midSnapshot = tracker.Snapshot()
	}

	r, err := newColdScanReparserFromFns(
		c.save, c.promote, &fakeGitQuerier{head: "h"},
		[]string{".go", ".ts", ".tsx"},
		WithIgnoreLoader(realIgnoreLoader),
		WithScanTracker(tracker),
	)
	if err != nil {
		t.Fatalf("newColdScanReparserFromFns: %v", err)
	}

	if got := tracker.Snapshot(); len(got) != 0 {
		t.Fatalf("pre-run tracker non-empty: %+v", got)
	}

	if err := r(context.Background(), RepoRecord{
		RepoID: "repo-z", RootPath: root, ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	if len(midSnapshot) != 1 || midSnapshot[0].RepoID != "repo-z" {
		t.Errorf("mid-scan snapshot = %+v, want one entry for repo-z", midSnapshot)
	}
	if got := tracker.Snapshot(); len(got) != 0 {
		t.Errorf("post-run tracker non-empty: %+v", got)
	}
}

func TestColdScanReparser_IndexesNonIgnoredFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")
	writeFile(t, root, "b.go", "package b")
	writeFile(t, root, "vendor/v.go", "package v")
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main")

	c := &captureFakes{}
	rep := newReparser(t, c, "deadbeef")

	if err := rep(context.Background(), RepoRecord{
		RepoID:       "r1",
		RootPath:     root,
		ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	got := c.savedPaths()
	// the parser is fed the repo-relative slash path, not the
	// absolute walked path.
	want := []string{"a.go", "b.go"}
	if !equalStringSlice(got, want) {
		t.Fatalf("saved paths = %v, want %v", got, want)
	}

	if len(c.promotes) != 1 {
		t.Fatalf("promotes = %d, want 1", len(c.promotes))
	}
	if c.promotes[0].SHA != "deadbeef" {
		t.Fatalf("promote SHA = %q, want deadbeef", c.promotes[0].SHA)
	}
}

// TestColdScanReparser_SkipsNonSourceNonManifest pins: the
// walker only feeds the ingester files the parsers can read (.go,.ts,
// tsx) or known manifests downstream checks gate on (go.mod). README,
// yml, etc. are skipped so staging isn't bloated with empty parse
// results.
func TestColdScanReparser_SkipsNonSourceNonManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")
	writeFile(t, root, "go.mod", "module example.com/x\n\ngo 1.22\n")
	writeFile(t, root, "README.md", "# hi")
	writeFile(t, root, "config.yaml", "k: v")
	writeFile(t, root, "data.json", `{"k":"v"}`)
	writeFile(t, root, "b.ts", "export const x = 1")

	c := &captureFakes{}
	rep := newReparser(t, c, "deadbeef")

	if err := rep(context.Background(), RepoRecord{
		RepoID:       "r1",
		RootPath:     root,
		ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	got := c.savedPaths()
	want := []string{"a.go", "b.ts", "go.mod"}
	if !equalStringSlice(got, want) {
		t.Fatalf("saved paths = %v, want %v (README/.yaml/.json must be filtered)", got, want)
	}
}

// TestColdScanReparser_LogsStartAndComplete pins: every scan
// emits a 'cold scan: starting' INFO at entry and a 'cold scan: complete'
// INFO at exit, with repo_id + git_sha + files_saved + elapsed. A newbie
// tailing ~/.veska/logs/daemon.log relies on these to know the scan is
// running and to see it finish.
func TestColdScanReparser_LogsStartAndComplete(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")
	writeFile(t, root, "b.go", "package b")

	c := &captureFakes{}
	rep := newReparser(t, c, "sha-test")
	if err := rep(context.Background(), RepoRecord{
		RepoID: "repo-x", RootPath: root, ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		`"cold scan: starting"`,
		`"cold scan: complete"`,
		`"repo_id":"repo-x"`,
		`"git_sha":"sha-test"`,
		`"files_saved":2`,
		`"elapsed_ms"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q; got:\n%s", want, out)
		}
	}
}

func TestColdScanReparser_RespectsVeskaIgnore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".veskaignore", "skip/\n")
	writeFile(t, root, "keep.go", "package k")
	writeFile(t, root, "skip/foo.go", "package s")

	c := &captureFakes{}
	rep := newReparser(t, c, "sha1")
	if err := rep(context.Background(), RepoRecord{
		RepoID: "r1", RootPath: root, ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	for _, s := range c.saves {
		if filepath.Base(filepath.Dir(s.Path)) == "skip" {
			t.Fatalf("unexpected save under skip/: %s", s.Path)
		}
	}
	if got := c.savedPaths(); len(got) != 1 || got[0] != "keep.go" {
		t.Fatalf("saved = %v, want [keep.go]", got)
	}
}

func TestColdScanReparser_Idempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")
	writeFile(t, root, "sub/b.go", "package b")

	c := &captureFakes{}
	rep := newReparser(t, c, "sha")
	repo := RepoRecord{RepoID: "r1", RootPath: root, ActiveBranch: "main"}

	if err := rep(context.Background(), repo); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	firstPaths := append([]string(nil), c.savedPaths()...)
	firstSaveCount := len(c.saves)
	firstPromotes := len(c.promotes)

	if err := rep(context.Background(), repo); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	c.mu.Lock()
	secondRun := c.saves[firstSaveCount:]
	c.mu.Unlock()
	got := make([]string, len(secondRun))
	for i, s := range secondRun {
		got[i] = s.Path
	}
	sort.Strings(got)
	if !equalStringSlice(got, firstPaths) {
		t.Fatalf("idempotent path set: run2=%v, run1=%v", got, firstPaths)
	}
	if len(c.promotes) != firstPromotes+1 {
		t.Fatalf("promotes after 2 runs = %d, want %d", len(c.promotes), firstPromotes+1)
	}
}

func TestColdScanReparser_PromotesAtHEAD(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a")

	c := &captureFakes{}
	rep := newReparser(t, c, "f00bar")
	if err := rep(context.Background(), RepoRecord{
		RepoID: "r1", RootPath: root, ActiveBranch: "main",
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	if len(c.promotes) != 1 {
		t.Fatalf("promotes = %d, want 1", len(c.promotes))
	}
	p := c.promotes[0]
	if p.SHA != "f00bar" {
		t.Fatalf("SHA = %q, want %q", p.SHA, "f00bar")
	}
	if p.Branch != "main" {
		t.Fatalf("Branch = %q, want main", p.Branch)
	}
	if p.Actor.Kind != domain.ActorKindSystem || p.Actor.ID != "service:veska" {
		t.Fatalf("Actor = %+v, want service:veska/system", p.Actor)
	}
}

func TestColdScanReparser_PropagatesCtxCancel(t *testing.T) {
	root := t.TempDir()
	for i := range 50 {
		writeFile(t, root, filepath.Join("dir", "f"+itoa(i)+".go"), "package x")
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &captureFakes{
		saveHook: func(_ context.Context, _, _, _ string, _ []byte) {
			cancel()
		},
	}
	rep := newReparser(t, c, "sha")
	err := rep(ctx, RepoRecord{RepoID: "r1", RootPath: root, ActiveBranch: "main"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(c.promotes) != 0 {
		t.Fatalf("promote called after cancel: %d times", len(c.promotes))
	}
	if n := len(c.saves); n == 0 || n >= 50 {
		t.Fatalf("save calls = %d, want > 0 and < 50", n)
	}
}

func TestNewColdScanReparser_NilDeps(t *testing.T) {
	area := staging.NewArea()
	gate := staging.NewGate(area)
	parser := &stubParser{result: &domain.ParseResult{}}
	ing := NewIngester(parser, area, gate)
	prom := NewPromoter(area, nil)
	git := &fakeGitQuerier{head: "x"}

	cases := []struct {
		name string
		ing  *Ingester
		pr   *Promoter
		g    GitQuerier
	}{
		{"nil ingester", nil, prom, git},
		{"nil promoter", ing, nil, git},
		{"nil git", ing, prom, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewColdScanReparser(tc.ing, tc.pr, tc.g)
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("err = %v, want ErrMissingDependency", err)
			}
		})
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
