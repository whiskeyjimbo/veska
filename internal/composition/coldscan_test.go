package composition

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
)

// CheckRunnerAdapter must satisfy the application.CheckRunner port the Promoter
// consumes; this is the contract both the daemon and the CLI cold-scan path
// rely on when wiring their check runner.
var _ application.CheckRunner = CheckRunnerAdapter{}

// GitAddedLinesFunc must surface the repo-root resolver's error unchanged so
// the Promoter can decide whether to skip diff-driven checks for the promotion.
func TestGitAddedLinesFunc_PropagatesRepoRootError(t *testing.T) {
	sentinel := errors.New("repo not registered")
	fn := GitAddedLinesFunc(func(context.Context, string) (string, error) {
		return "", sentinel
	})

	_, err := fn(context.Background(), "repo-1", "deadbeef")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected repo-root error to propagate, got %v", err)
	}
}
