package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/whiskeyjimbo/veska/internal/repo"
)

// promptDeps is the seam tests use to fake isatty + stdin (solov2-kxo5.7).
// In production both are derived from os.Stdin / os.Stdout.
type promptDeps struct {
	isTTY  func() bool
	stdin  io.Reader
	stdout io.Writer
}

func defaultPromptDeps(w io.Writer) promptDeps {
	return promptDeps{
		isTTY: func() bool {
			// TTY-only per design: BOTH stdin and stdout must be terminals
			// so we never block on a closed pipe or paint a prompt at a
			// file that nobody is watching.
			return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		},
		stdin:  os.Stdin,
		stdout: w,
	}
}

// runAcceptancePrompt presents the "keep this indexed?" UX after a
// search against an ephemeral repo (solov2-kxo5.7).
//
//   - prompted_at already set → silent no-op (AC3: once per row lifetime)
//   - not a TTY → print one-liner hint, leave row ephemeral, do not
//     mark prompted_at (so a future TTY invocation can still ask)
//   - TTY + unprompted → ask y/N; y promotes via PromoteEphemeralToTracked,
//     n marks declined via MarkPromptDeclined. Promotion is in-place: no
//     re-clone, no file move (AC4).
func runAcceptancePrompt(ctx context.Context, db *sql.DB, rec repo.Record, canonicalURL string, deps promptDeps) error {
	if rec.Kind != "ephemeral" {
		return nil
	}
	var prompted sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT prompted_at FROM repos WHERE repo_id = ?`, rec.RepoID,
	).Scan(&prompted); err != nil {
		return fmt.Errorf("acceptance prompt: read prompted_at: %w", err)
	}
	if prompted.Valid {
		return nil
	}

	if !deps.isTTY() {
		fmt.Fprintf(deps.stdout, "\nto keep this indexed, run: veska repo add %s\n", canonicalURL)
		return nil
	}

	fmt.Fprintf(deps.stdout, "\nkeep %s indexed? [y/N] ", canonicalURL)
	reader := bufio.NewReader(deps.stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("acceptance prompt: read response: %w", err)
	}
	resp := strings.ToLower(strings.TrimSpace(line))
	switch resp {
	case "y", "yes":
		if err := repo.PromoteEphemeralToTracked(ctx, db, rec.RepoID); err != nil {
			return err
		}
		fmt.Fprintf(deps.stdout, "promoted %s to tracked\n", shortRepoID(rec.RepoID))
	default:
		if err := repo.MarkPromptDeclined(ctx, db, rec.RepoID); err != nil {
			return err
		}
	}
	return nil
}
