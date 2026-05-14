package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// stubRepoLister is an in-test implementation of RepoLister.
type stubRepoLister struct {
	repos []RepoRecord
	err   error
}

func (s *stubRepoLister) ListRepos(_ context.Context) ([]RepoRecord, error) {
	return s.repos, s.err
}

// stubGitQuerier is an in-test implementation of GitQuerier.
type stubGitQuerier struct {
	headFn         func(rootPath string) (string, error)
	isAncestorFn   func(rootPath, sha, head string) (bool, error)
	commitsSinceFn func(rootPath, sha, head string) ([]string, error)
	changedFilesFn func(rootPath, sha string) ([]string, error)
	readFileAtFn   func(rootPath, sha, filePath string) ([]byte, error)
}

func (s *stubGitQuerier) HEAD(rootPath string) (string, error) {
	if s.headFn != nil {
		return s.headFn(rootPath)
	}
	return "HEAD-SHA", nil
}

func (s *stubGitQuerier) IsAncestor(rootPath, sha, head string) (bool, error) {
	if s.isAncestorFn != nil {
		return s.isAncestorFn(rootPath, sha, head)
	}
	return true, nil
}

func (s *stubGitQuerier) CommitsSince(rootPath, sha, head string) ([]string, error) {
	if s.commitsSinceFn != nil {
		return s.commitsSinceFn(rootPath, sha, head)
	}
	return nil, nil
}

func (s *stubGitQuerier) ChangedFiles(rootPath, sha string) ([]string, error) {
	if s.changedFilesFn != nil {
		return s.changedFilesFn(rootPath, sha)
	}
	return nil, nil
}

func (s *stubGitQuerier) ReadFileAtCommit(rootPath, sha, filePath string) ([]byte, error) {
	if s.readFileAtFn != nil {
		return s.readFileAtFn(rootPath, sha, filePath)
	}
	return []byte{}, nil
}

// callTracker records Save and Promote call arguments.
type callTracker struct {
	mu           sync.Mutex
	saveCalls    []saveCall
	promoteCalls []promoteCall
	saveErr      error
	promoteErr   error
}

type saveCall struct {
	repoID, branch, path string
	src                  []byte
}

type promoteCall struct {
	repoID, branch, gitSHA string
}

func (c *callTracker) saveFunc() func(ctx context.Context, repoID, branch, path string, src []byte) error {
	return func(ctx context.Context, repoID, branch, path string, src []byte) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.saveCalls = append(c.saveCalls, saveCall{repoID, branch, path, src})
		return c.saveErr
	}
}

func (c *callTracker) promoteFunc() func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
	return func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.promoteCalls = append(c.promoteCalls, promoteCall{repoID, branch, gitSHA})
		return c.promoteErr
	}
}

// newTestResync builds a StartupResync with stub injectors instead of real Ingester/Promoter.
func newTestResync(
	repos RepoLister,
	git GitQuerier,
	saveFn func(ctx context.Context, repoID, branch, path string, src []byte) error,
	promoteFn func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error,
	reparserFn func(ctx context.Context, repo RepoRecord) error,
) *StartupResync {
	return &StartupResync{
		repos:    repos,
		git:      git,
		save:     saveFn,
		promote:  promoteFn,
		reparser: reparserFn,
	}
}

// TestResync_AlreadyAtHEAD verifies that when last_promoted_sha == HEAD,
// no ingester/promoter calls are made.
func TestResync_AlreadyAtHEAD(t *testing.T) {
	const headSHA = "abc123"
	repos := &stubRepoLister{repos: []RepoRecord{{
		RepoID:          "repo1",
		RootPath:        "/tmp/repo1",
		ActiveBranch:    "main",
		LastPromotedSHA: headSHA,
	}}}
	git := &stubGitQuerier{
		headFn: func(_ string) (string, error) { return headSHA, nil },
	}
	tracker := &callTracker{}
	var reparserCalled bool

	sr := newTestResync(repos, git, tracker.saveFunc(), tracker.promoteFunc(),
		func(_ context.Context, _ RepoRecord) error {
			reparserCalled = true
			return nil
		})

	if err := sr.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.saveCalls) != 0 {
		t.Errorf("expected 0 Save calls, got %d", len(tracker.saveCalls))
	}
	if len(tracker.promoteCalls) != 0 {
		t.Errorf("expected 0 Promote calls, got %d", len(tracker.promoteCalls))
	}
	if reparserCalled {
		t.Error("expected reparser NOT to be called when already at HEAD")
	}
}

// TestResync_MissedCommits verifies that when last_promoted_sha is reachable
// from HEAD, Save + Promote are called per commit.
func TestResync_MissedCommits(t *testing.T) {
	const (
		lastSHA = "old-sha"
		headSHA = "new-sha"
	)
	commits := []string{"commit-1", "commit-2"}
	commitFiles := map[string][]string{
		"commit-1": {"a.go"},
		"commit-2": {"b.go"},
	}

	repos := &stubRepoLister{repos: []RepoRecord{{
		RepoID:          "repo1",
		RootPath:        "/tmp/repo1",
		ActiveBranch:    "main",
		LastPromotedSHA: lastSHA,
	}}}
	git := &stubGitQuerier{
		headFn:         func(_ string) (string, error) { return headSHA, nil },
		isAncestorFn:   func(_, _, _ string) (bool, error) { return true, nil },
		commitsSinceFn: func(_, _, _ string) ([]string, error) { return commits, nil },
		changedFilesFn: func(_, sha string) ([]string, error) { return commitFiles[sha], nil },
		readFileAtFn:   func(_, _, _ string) ([]byte, error) { return []byte("content"), nil },
	}
	tracker := &callTracker{}
	var reparserCalled bool

	sr := newTestResync(repos, git, tracker.saveFunc(), tracker.promoteFunc(),
		func(_ context.Context, _ RepoRecord) error {
			reparserCalled = true
			return nil
		})

	if err := sr.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	// 2 commits × 1 file each = 2 Save calls
	if len(tracker.saveCalls) != 2 {
		t.Errorf("expected 2 Save calls, got %d", len(tracker.saveCalls))
	}
	// 2 Promote calls — one per commit
	if len(tracker.promoteCalls) != 2 {
		t.Errorf("expected 2 Promote calls, got %d", len(tracker.promoteCalls))
	}
	if reparserCalled {
		t.Error("reparser should NOT be called for reachable SHA")
	}

	// Verify promote SHAs match commits (oldest first)
	for i, pc := range tracker.promoteCalls {
		if pc.gitSHA != commits[i] {
			t.Errorf("promote[%d] gitSHA: want %q, got %q", i, commits[i], pc.gitSHA)
		}
	}
}

// TestResync_DivergentSHA verifies that a non-ancestor SHA triggers the reparser
// and that Run returns nil (divergent errors are non-fatal).
func TestResync_DivergentSHA(t *testing.T) {
	repos := &stubRepoLister{repos: []RepoRecord{{
		RepoID:          "repo1",
		RootPath:        "/tmp/repo1",
		ActiveBranch:    "main",
		LastPromotedSHA: "force-pushed-away",
	}}}
	git := &stubGitQuerier{
		headFn:       func(_ string) (string, error) { return "new-tip", nil },
		isAncestorFn: func(_, _, _ string) (bool, error) { return false, nil },
	}
	tracker := &callTracker{}
	var reparserCalled bool

	sr := newTestResync(repos, git, tracker.saveFunc(), tracker.promoteFunc(),
		func(_ context.Context, _ RepoRecord) error {
			reparserCalled = true
			return nil
		})

	if err := sr.Run(context.Background()); err != nil {
		t.Fatalf("Run must return nil for divergent SHA (non-fatal), got: %v", err)
	}

	if !reparserCalled {
		t.Error("expected reparser to be called for divergent SHA")
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.saveCalls) != 0 {
		t.Errorf("expected no Save calls for divergent path, got %d", len(tracker.saveCalls))
	}
}

// TestResync_NeverPromoted verifies that an empty LastPromotedSHA triggers
// the reparser directly.
func TestResync_NeverPromoted(t *testing.T) {
	repos := &stubRepoLister{repos: []RepoRecord{{
		RepoID:       "repo1",
		RootPath:     "/tmp/repo1",
		ActiveBranch: "main",
		// LastPromotedSHA intentionally empty
	}}}
	git := &stubGitQuerier{
		headFn: func(_ string) (string, error) { return "some-sha", nil },
	}
	tracker := &callTracker{}
	var reparserCalled bool

	sr := newTestResync(repos, git, tracker.saveFunc(), tracker.promoteFunc(),
		func(_ context.Context, _ RepoRecord) error {
			reparserCalled = true
			return nil
		})

	if err := sr.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if !reparserCalled {
		t.Error("expected reparser to be called for never-promoted repo")
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.saveCalls) != 0 {
		t.Errorf("expected no Save calls for never-promoted repo, got %d", len(tracker.saveCalls))
	}
	if len(tracker.promoteCalls) != 0 {
		t.Errorf("expected no Promote calls for never-promoted repo, got %d", len(tracker.promoteCalls))
	}
}

// TestResync_IsSyncing verifies IsSyncing is true during Run and false after.
func TestResync_IsSyncing(t *testing.T) {
	const headSHA = "abc123"
	repos := &stubRepoLister{repos: []RepoRecord{{
		RepoID:          "repo1",
		RootPath:        "/tmp/repo1",
		ActiveBranch:    "main",
		LastPromotedSHA: headSHA, // already up to date — but Run still runs
	}}}
	git := &stubGitQuerier{
		headFn: func(_ string) (string, error) { return headSHA, nil },
	}

	var isSyncingDuringRun atomic.Bool
	// We observe IsSyncing from within the reparser/save path.
	// Use a blocking reparser to snapshot the flag while Run is in flight.
	block := make(chan struct{})
	unblock := make(chan struct{})

	// Use a repo that requires reparse so we can block inside reparser.
	reposMissed := &stubRepoLister{repos: []RepoRecord{{
		RepoID:       "repo2",
		RootPath:     "/tmp/repo2",
		ActiveBranch: "main",
		// empty SHA → full reparse
	}}}
	gitMissed := &stubGitQuerier{
		headFn: func(_ string) (string, error) { return "tip", nil },
	}

	var srPtr *StartupResync
	sr := newTestResync(reposMissed, gitMissed,
		func(_ context.Context, _, _, _ string, _ []byte) error { return nil },
		func(_ context.Context, _, _, _ string, _ domain.Actor) error { return nil },
		func(_ context.Context, _ RepoRecord) error {
			isSyncingDuringRun.Store(srPtr.IsSyncing())
			close(block)
			<-unblock
			return nil
		},
	)
	srPtr = sr

	done := make(chan error, 1)
	go func() { done <- sr.Run(context.Background()) }()

	<-block // wait until inside reparser
	if !isSyncingDuringRun.Load() {
		t.Error("IsSyncing should be true during Run")
	}
	close(unblock)

	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if sr.IsSyncing() {
		t.Error("IsSyncing should be false after Run completes")
	}

	_ = repos
	_ = git
}

// TestResync_ErrPromotionDivergent verifies the error message format.
func TestResync_ErrPromotionDivergent_Error(t *testing.T) {
	err := &ErrPromotionDivergent{RepoID: "my-repo", SHA: "deadbeef"}
	got := err.Error()
	if got == "" {
		t.Fatal("Error() returned empty string")
	}
	// Must mention the repo and SHA.
	for _, want := range []string{"my-repo", "deadbeef"} {
		if !containsStr(got, want) {
			t.Errorf("Error() = %q; want it to contain %q", got, want)
		}
	}
}

// TestResync_ErrDaemonStarting verifies the sentinel error is defined.
func TestResync_ErrDaemonStarting(t *testing.T) {
	if ErrDaemonStarting == nil {
		t.Fatal("ErrDaemonStarting must not be nil")
	}
	if !errors.Is(ErrDaemonStarting, ErrDaemonStarting) {
		t.Fatal("ErrDaemonStarting must satisfy errors.Is against itself")
	}
}

// containsStr is a tiny helper because strings.Contains is in strings package.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
