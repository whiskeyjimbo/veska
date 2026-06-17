package composition

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
)

var _ application.CheckRunner = CheckRunnerAdapter{}

// GitAddedLinesFunc must propagate repository root resolver errors unchanged.
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
