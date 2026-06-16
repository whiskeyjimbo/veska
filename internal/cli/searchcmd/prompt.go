package searchcmd

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// RunAcceptancePrompt presents the "keep this indexed?" UX after a search
// against an ephemeral repo.
//
//	prompted_at already set → silent no-op (AC3: once per row lifetime)
//	not a TTY → print one-liner hint, leave row ephemeral, do not mark
//	  prompted_at (so a future TTY invocation can still ask)
//	TTY + unprompted → ask y/N; y promotes via PromoteEphemeralToTracked, n
//	  marks declined via MarkPromptDeclined. Promotion is in-place: no
//	  re-clone, no file move (AC4).
func RunAcceptancePrompt(ctx context.Context, db *sql.DB, rec repo.Record, canonicalURL string, deps repocmd.PromptDeps) error {
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

	if !deps.IsTTY() {
		fmt.Fprintf(deps.Stdout, "\nto keep this indexed, run: veska repo add %s\n", canonicalURL)
		return nil
	}

	fmt.Fprintf(deps.Stdout, "\nkeep %s indexed? [y/N] ", canonicalURL)
	reader := bufio.NewReader(deps.Stdin)
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
		fmt.Fprintf(deps.Stdout, "promoted %s to tracked\n", repocmd.ShortRepoID(rec.RepoID))
	default:
		if err := repo.MarkPromptDeclined(ctx, db, rec.RepoID); err != nil {
			return err
		}
	}
	return nil
}
