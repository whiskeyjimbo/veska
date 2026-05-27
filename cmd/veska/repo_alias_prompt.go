package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/repo"
)

// runAliasSuggestPrompt offers an auto-suggested alias on `veska repo add`
// (solov2-7w1t). TTY-only — non-TTY callers skip silently so scripts /
// MCP-driven adds don't block.
//
// UX:
//   - y → bind the suggested name
//   - N (or anything starting with n) → skip
//   - anything else → treat the response as a custom alias name
//
// If the suggested name collides with an existing alias and a fallback
// exists (URL form: "<owner>-<name>"), the prompt tries the fallback. If
// that also collides, the prompt is skipped — the user can still bind one
// by hand via `veska repo alias`.
func runAliasSuggestPrompt(ctx context.Context, db *sql.DB, repoID, canonicalURL, rootPath string, deps promptDeps) error {
	if !deps.isTTY() {
		return nil
	}

	primary, fallback := repo.SuggestAliasNames(canonicalURL, rootPath)
	suggested, err := pickFreeAlias(ctx, db, primary, fallback)
	if err != nil {
		return err
	}
	if suggested == "" {
		return nil
	}

	fmt.Fprintf(deps.stdout, "\nalias this repo to %q? [y/N/<custom>] ", suggested)
	reader := bufio.NewReader(deps.stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("alias prompt: read response: %w", err)
	}
	resp := strings.TrimSpace(line)
	lower := strings.ToLower(resp)
	switch {
	case lower == "" || lower == "n" || lower == "no":
		return nil
	case lower == "y" || lower == "yes":
		// Bind the suggested name.
	default:
		// Anything else is taken as a custom name.
		suggested = resp
	}

	if err := repo.SetAlias(ctx, db, suggested, repoID, false); err != nil {
		if errors.Is(err, repo.ErrAliasExists) || errors.Is(err, repo.ErrAliasInvalid) {
			fmt.Fprintf(deps.stdout, "alias not set: %v\n", err)
			return nil
		}
		return err
	}
	fmt.Fprintf(deps.stdout, "aliased %q to %s\n", suggested, shortRepoID(repoID))
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
