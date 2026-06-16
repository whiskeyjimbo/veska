package repocmd

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// PromptDeps is the seam tests use to fake isatty + stdin.
// In production both are derived from os.Stdin / os.Stdout.
type PromptDeps struct {
	IsTTY  func() bool
	Stdin  io.Reader
	Stdout io.Writer
}

// DefaultPromptDeps wires the production isatty + stdin/stdout into PromptDeps.
func DefaultPromptDeps(w io.Writer) PromptDeps {
	return PromptDeps{
		IsTTY: func() bool {
			// TTY-only per design: BOTH stdin and stdout must be terminals
			// so we never block on a closed pipe or paint a prompt at a
			// file that nobody is watching.
			return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		},
		Stdin:  os.Stdin,
		Stdout: w,
	}
}

// AliasTarget identifies the repo a suggest-prompt may alias: its canonical
// repo_id plus the URL/path the suggested name is derived from.
type AliasTarget struct {
	RepoID       string
	CanonicalURL string
	RootPath     string
}

// RunAliasSuggestPrompt offers an auto-suggested alias on `veska repo add`
// TTY-only — non-TTY callers skip silently so scripts /
// MCP-driven adds don't block.
// UX:
//
//	y → bind the suggested name
//	N (or anything starting with n) → skip
//	anything else → treat the response as a custom alias name
//
// If the suggested name collides with an existing alias and a fallback
// exists (URL form: "<owner>-<name>"), the prompt tries the fallback. If
// that also collides, the prompt is skipped — the user can still bind one
// by hand via `veska repo alias`.
func RunAliasSuggestPrompt(ctx context.Context, db *sql.DB, target AliasTarget, deps PromptDeps) error {
	if !deps.IsTTY() {
		return nil
	}

	primary, fallback := repo.SuggestAliasNames(target.CanonicalURL, target.RootPath)
	suggested, err := pickFreeAlias(ctx, db, primary, fallback)
	if err != nil {
		return err
	}
	if suggested == "" {
		return nil
	}

	fmt.Fprintf(deps.Stdout, "\nalias this repo to %q? [y/N/<custom>] ", suggested)
	reader := bufio.NewReader(deps.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("alias prompt: read response: %w", err)
	}
	resp := strings.TrimSpace(line)
	switch strings.ToLower(resp) {
	case "", "n", "no":
		return nil
	case "y", "yes":
		// Bind the suggested name.
	default:
		// Anything else is taken as a custom name.
		suggested = resp
	}

	if err := repo.SetAlias(ctx, db, suggested, target.RepoID, false); err != nil {
		if errors.Is(err, repo.ErrAliasExists) || errors.Is(err, repo.ErrAliasInvalid) {
			fmt.Fprintf(deps.Stdout, "alias not set: %v\n", err)
			return nil
		}
		return err
	}
	fmt.Fprintf(deps.Stdout, "aliased %q to %s\n", suggested, ShortRepoID(target.RepoID))
	return nil
}

// pickFreeAlias returns the first of primary/fallback that is not already
// bound. An empty primary yields "". When both are bound the function
// returns "" so the caller skips the prompt.
func pickFreeAlias(ctx context.Context, db *sql.DB, primary, fallback string) (string, error) {
	for _, candidate := range []string{primary, fallback} {
		if candidate == "" {
			continue
		}
		_, exists, err := repo.LookupAlias(ctx, db, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", nil
}
