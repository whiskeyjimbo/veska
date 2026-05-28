package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// repoCmd returns the "repo" Cobra command with "add" and "remove" sub-commands.
// Both sub-commands prefer the running daemon's MCP socket (so they go through
// repoRegistrar.AddRepo / RemoveRepo and pick up the cold-scan + live-watch
// wiring) and fall back to a direct SQLite write when the daemon is unreachable.
func repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "repo",
		Short:        "Manage git repositories tracked by veska",
		SilenceUsage: true,
	}
	cmd.AddCommand(repoAddCmd())
	cmd.AddCommand(repoRemoveCmd())
	cmd.AddCommand(repoListCmd())
	cmd.AddCommand(repoPruneCmd())
	cmd.AddCommand(repoAliasCmd())
	cmd.AddCommand(repoUnaliasCmd())
	return cmd
}

// repoAliasCmd: set a user-defined human-friendly name for a repo
// (solov2-7w1t). Resolves the supplied id against the same progression
// as every other repo-targeted command: full id, short_id, alias, prefix.
// --force overwrites an existing alias bound to a different repo so the
// user gets a loud confirmation prompt by default.
func repoAliasCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:          "alias <name> <repo-id-or-prefix-or-alias>",
		Short:        "Bind a human-friendly name to a repo",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name, target := args[0], args[1]

			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo alias: %w", err)
			}
			defer closeFn()

			recs, err := repo.List(ctx, db)
			if err != nil {
				return fmt.Errorf("repo alias: %w", err)
			}
			rec, err := resolveCLIRepoID(recs, target)
			if err != nil {
				// Likely arg-order mistake: `veska repo alias <id> <name>`
				// instead of the documented `<name> <id>`. If args[0]
				// resolves and args[1] does not, surface the swap hint
				// instead of the generic not-registered error (solov2-fdni).
				if _, swapErr := resolveCLIRepoID(recs, name); swapErr == nil {
					return fmt.Errorf("repo alias: %w — did you swap the arguments? usage: `veska repo alias <name> <repo-id-or-prefix-or-alias>` (got name=%q repo=%q)", err, name, target)
				}
				return fmt.Errorf("repo alias: %w", err)
			}
			if err := repo.SetAlias(ctx, db, name, rec.RepoID, force); err != nil {
				if errors.Is(err, repo.ErrAliasExists) {
					return fmt.Errorf("repo alias: %w (re-run with --force to overwrite)", err)
				}
				return fmt.Errorf("repo alias: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "aliased %q to %s\n", name, shortRepoID(rec.RepoID))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing alias bound to a different repo")
	return cmd
}

// repoUnaliasCmd: remove a user-defined alias (solov2-7w1t). Errors on
// unknown name so a typo doesn't silently succeed.
func repoUnaliasCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "unalias <name>",
		Short:        "Remove a user-defined alias",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo unalias: %w", err)
			}
			defer closeFn()
			if err := repo.RemoveAlias(ctx, db, args[0]); err != nil {
				return fmt.Errorf("repo unalias: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed alias %q\n", args[0])
			return nil
		},
	}
}

// repoPruneCmd is the deprecated alias for `repo remove --missing`. Hidden
// from help but kept for one release so existing scripts/muscle memory keep
// working. solov2-meuk: cleanup verbs were split, junior users had no way
// to know which one applied. Remove this command after one release cycle.
func repoPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Deprecated: use `veska repo remove --missing`",
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "`veska repo prune` is deprecated; use `veska repo remove --missing`")
			return removeMissingRepos(cmd.Context(), cmd.OutOrStdout(), dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list candidates without removing them")
	return cmd
}

// repoView is the row shape used by both the daemon path (decoded from
// eng_list_repos) and the direct-DB fallback path. Field names match the
// MCP response so json.Unmarshal works as-is.
type repoView struct {
	RepoID          string `json:"repo_id"`
	RootPath        string `json:"root_path"`
	ActiveBranch    string `json:"active_branch"`
	LastPromotedSHA string `json:"last_promoted_sha"`
	Kind            string `json:"kind"` // solov2-kxo5.9
	// Aliases is the list of user-defined human-friendly names for this
	// repo (solov2-7w1t). Surfaced in the ALIAS column of `veska repo
	// list` and accepted anywhere a repo_id is expected.
	Aliases []string `json:"aliases"`
}

// repoListCmd prints every registered repo (solov2-0pq). Prefers the
// running daemon's eng_list_repos so the listing matches what the daemon
// sees (including any in-flight scan state surfaced via degraded_reasons);
// falls back to a direct SQLite read so the CLI still works when the
// daemon is down.
func repoListCmd() *cobra.Command {
	var includeExternal bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List registered git repositories",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()

			type listResult struct {
				Repos []repoView `json:"repos"`
			}
			var lr listResult
			params := map[string]any{}
			if includeExternal {
				params["include_vendored"] = true
			}
			if err := callMCP(ctx, "eng_list_repos", params, &lr); err == nil {
				progress := fetchScanProgress(ctx)
				printRepoTableWithProgress(w, lr.Repos, progress)
				return nil
			}

			// Direct fallback.
			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo list: %w", err)
			}
			defer closeFn()
			recs, err := repo.List(ctx, db)
			if err != nil {
				return fmt.Errorf("repo list: %w", err)
			}
			views := make([]repoView, 0, len(recs))
			for _, r := range recs {
				if !includeExternal && strings.HasPrefix(r.RepoID, extindex.SyntheticRepoIDPrefix) {
					continue
				}
				views = append(views, repoView{
					RepoID:          r.RepoID,
					RootPath:        r.RootPath,
					ActiveBranch:    r.ActiveBranch,
					LastPromotedSHA: r.LastPromotedSHA,
					Kind:            r.Kind,
					Aliases:         r.Aliases,
				})
			}
			printRepoTable(w, views)
			return nil
		},
	}
	cmd.Flags().BoolVar(&includeExternal, "include-external", false,
		"also show synthetic ext:<module> repos created by `veska deps index`")
	return cmd
}

// shortRepoID returns the first 12 chars of a repo id — the alias shown by
// `veska repo list` and accepted anywhere a repo_id is required. The CLI
// surfaces this form so users copy the same token the tools expect, instead
// of the unwieldy 64-char canonical id (solov2-ow4b).
func shortRepoID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cliMinRepoIDPrefix mirrors mcp.minRepoIDPrefix — see that constant for the
// reasoning (solov2-rkbc).
const cliMinRepoIDPrefix = 4

// resolveCLIRepoID matches the MCP resolveRepoID progression for CLI callers:
// exact full id, then 12-char short_id, then user-set alias (solov2-7w1t),
// then unambiguous prefix (>= 4 chars). Aliases beat prefix so a typed
// alias never gets shadowed by a colliding hex prefix.
// Returns a typed error so CLI commands can wrap it with their own prefix
// ("wiki: ", "reindex: ", etc.) (solov2-c7lq).
func resolveCLIRepoID(records []repo.Record, repoID string) (repo.Record, error) {
	for _, r := range records {
		if r.RepoID == repoID {
			return r, nil
		}
	}
	for _, r := range records {
		if shortRepoID(r.RepoID) == repoID {
			return r, nil
		}
	}
	for _, r := range records {
		if slices.Contains(r.Aliases, repoID) {
			return r, nil
		}
	}
	if len(repoID) >= cliMinRepoIDPrefix {
		var matched repo.Record
		found := false
		for _, r := range records {
			if strings.HasPrefix(r.RepoID, repoID) {
				if found {
					return repo.Record{}, fmt.Errorf("ambiguous repo_id prefix %q matches multiple repos", repoID)
				}
				matched, found = r, true
			}
		}
		if found {
			return matched, nil
		}
	}
	if len(repoID) < cliMinRepoIDPrefix {
		return repo.Record{}, fmt.Errorf("repo %q is not registered (prefixes must be >= %d chars)", repoID, cliMinRepoIDPrefix)
	}
	return repo.Record{}, fmt.Errorf("repo %q is not registered (no match by full id, short_id, alias, or prefix)", repoID)
}

// scanProgressRow is the per-scan progress snapshot surfaced into repo
// list — phase ("walking" / "promoting") + files_seen — so a user can
// tell the sub-second walk from the long promotion phase that follows
// it (solov2-u9h9).
type scanProgressRow struct {
	Phase     string
	FilesSeen int
	StartedAt time.Time
}

// fetchScanProgress pulls scans_in_flight from eng_get_status and returns
// a map repo_id → progress. Best-effort: nil if the call fails or the
// daemon is too old to surface the fields.
func fetchScanProgress(ctx context.Context) map[string]scanProgressRow {
	var status struct {
		ScansInFlight []struct {
			RepoID    string    `json:"repo_id"`
			Phase     string    `json:"phase"`
			FilesSeen int       `json:"files_seen"`
			StartedAt time.Time `json:"started_at"`
		} `json:"scans_in_flight"`
	}
	if err := callMCP(ctx, "eng_get_status", map[string]any{}, &status); err != nil {
		return nil
	}
	m := make(map[string]scanProgressRow, len(status.ScansInFlight))
	for _, s := range status.ScansInFlight {
		m[s.RepoID] = scanProgressRow{
			Phase:     s.Phase,
			FilesSeen: s.FilesSeen,
			StartedAt: s.StartedAt,
		}
	}
	return m
}

// formatScanElapsed renders ScanState.StartedAt → "1m23s" / "12s". A zero
// StartedAt yields "" so older daemons (no started_at in scans_in_flight)
// quietly omit the suffix.
func formatScanElapsed(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	d := time.Since(startedAt)
	if d < 0 {
		return ""
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%ds", mins, secs)
}

// printRepoTable renders the repo list as REPO_ID + ROOT + BRANCH + STATUS.
// A short repo_id (first 12 chars) is shown so the column is readable; the
// full id is still present in any tool output, and `veska repo remove`
// accepts the full id.
func printRepoTable(w io.Writer, repos []repoView) {
	printRepoTableWithProgress(w, repos, nil)
}

// printRepoTableWithProgress overlays in-flight scan progress onto the
// (unindexed) rows so a user watching a long cold scan can tell hung
// from progressing (solov2-u9h9). progress maps repo_id → phase + files_seen.
func printRepoTableWithProgress(w io.Writer, repos []repoView, progress map[string]scanProgressRow) {
	if len(repos) == 0 {
		fmt.Fprintln(w, "no repositories registered — run: veska repo add <path>")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO_ID\tKIND\tALIAS\tBRANCH\tSTATUS\tROOT")
	for _, r := range repos {
		short := shortRepoID(r.RepoID)
		branch := r.ActiveBranch
		if branch == "" {
			branch = "-"
		}
		kind := r.Kind
		if kind == "" {
			kind = "tracked"
		}
		alias := "-"
		if len(r.Aliases) > 0 {
			alias = strings.Join(r.Aliases, ",")
		}
		status := "promoted"
		if r.LastPromotedSHA == "" {
			status = "(unindexed)"
			// solov2-jtl5.8: a never-promoted repo isn't always 'just hasn't
			// scanned yet'. A failed cold-scan leaves the repo in this state
			// too, and the user has no signal until they tail daemon.log. If
			// the most recent ERROR/WARN line in the log names this repo, surface
			// 'scan failed' instead of the silently-misleading '(unindexed)'.
			if _, inFlight := progress[r.RepoID]; !inFlight {
				logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
				if reason := tailScanFailureReason(logPath, r.RepoID); reason != "" {
					status = "(scan failed)"
				}
			}
			if p, ok := progress[r.RepoID]; ok {
				elapsed := formatScanElapsed(p.StartedAt)
				switch {
				case p.Phase == "promoting":
					status = fmt.Sprintf("(promoting, %d files)", p.FilesSeen)
				case p.Phase == "walking" && p.FilesSeen > 0:
					status = fmt.Sprintf("(walking, %d files)", p.FilesSeen)
				case p.Phase != "":
					status = fmt.Sprintf("(%s)", p.Phase)
				case p.FilesSeen > 0:
					status = fmt.Sprintf("(scanning, %d files)", p.FilesSeen)
				}
				// solov2-jtl5.1: append elapsed so a user can tell a slow-but-
				// progressing scan from a hung one even when files_seen plateaus
				// on a single large file. Older daemons omit started_at and the
				// suffix is suppressed.
				if elapsed != "" && status != "(unindexed)" {
					status = status[:len(status)-1] + ", " + elapsed + ")"
				}
			}
		}
		// Flag repos whose root path no longer exists on disk so users can see
		// stale registrations at a glance (solov2-76px). `repo remove <id>` is
		// still the cleanup path.
		if r.RootPath != "" {
			if _, err := os.Stat(r.RootPath); errors.Is(err, fs.ErrNotExist) {
				status = "(missing)"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", short, kind, alias, branch, status, r.RootPath)
	}
	_ = tw.Flush()
}

// coldScanRunningHint returns the post-`repo add` hint shown when the daemon
// has accepted a new repo and is cold-scanning it asynchronously. The hint
// must name `veska repo add <path> --wait` explicitly — `--wait` is a flag on
// `repo add`, not on `repo list`, and a copy-pasteable suggestion avoids the
// solov2-rhaq trap where juniors run `veska repo list --wait` and hit
// "unknown flag".
func coldScanRunningHint(root, logPath string) string {
	return fmt.Sprintf("  cold scan running in the background — `veska repo list` shows status; re-run with `veska repo add %s --wait` to block until it finishes, or `tail %s` for live progress", root, logPath)
}

func repoAddCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:          "add <path-or-url>",
		Short:        "Register a git repository (local path or remote URL) and install hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			root := args[0]

			// URL form: clone to the tracked tier and run the rest of
			// `repo add` against the cloned path (solov2-kxo5.3).
			if looksLikeRepoURL(root) {
				return runRepoAddURL(ctx, cmd, root, wait)
			}

			// solov2-clgn: '.', '..', or any relative path must be resolved
			// against the user's cwd here. The daemon's cwd is unrelated and
			// would otherwise mis-resolve '.' to the daemon's working dir.
			abs, err := filepath.Abs(root)
			if err != nil {
				return fmt.Errorf("repo add: resolve %q: %w", root, err)
			}
			root = abs

			// Prefer the daemon when up — it triggers cold scan and seeds
			// the live watcher in one call (parity with eng_add_repo).
			id, existed, dialErr := dialAddRepo(ctx, root)
			if dialErr == nil {
				if existed {
					fmt.Fprintf(w, "repo already registered: %s (via daemon)\n", shortRepoID(id))
					return nil
				}
				fmt.Fprintf(w, "added repo %s (via daemon)\n", shortRepoID(id))
				if !wait {
					promptAliasAfterAdd(ctx, w, id, "", root)
				}
				if wait {
					return waitForScanComplete(ctx, w, id)
				}
				logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
				fmt.Fprintln(w, coldScanRunningHint(root, logPath))
				return nil
			}

			// solov2-qnt9: --wait promises to block until cold scan
			// completes; when the daemon is unreachable there IS no cold
			// scan to wait on, so silently degrading to the direct-write
			// fallback would make the flag a lie. Surface a clear error
			// with the exact daemon-start command so the user can recover
			// in one step. The non-wait path keeps its bootstrap-friendly
			// fallback below.
			if wait {
				return fmt.Errorf("repo add --wait: daemon unreachable (%v); start it with `veska service start` and re-run, or drop --wait to register the repo offline (next daemon start will cold-scan it)", dialErr)
			}

			// Direct fallback: insert the row + install hooks. The next
			// daemon start will cold-scan it via StartupResync; live-watching
			// kicks in at that point too. Surface the dial error so the user
			// can tell 'daemon really is down' from 'daemon up but I can't
			// reach it' (solov2-0cg).
			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo add: %w", err)
			}
			defer closeFn()

			var existedLocal bool
			id, existedLocal, err = repo.Add(ctx, db, root)
			if err != nil {
				return fmt.Errorf("repo add: %w", err)
			}
			verb := "added"
			if existedLocal {
				verb = "already registered"
			}
			fmt.Fprintf(w, "%s repo %s (direct write; daemon dial failed: %v — restart daemon to cold-scan/live-watch)\n", verb, shortRepoID(id), dialErr)
			if !existedLocal {
				promptAliasAfterAdd(ctx, w, id, "", root)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the cold scan completes; print live progress")
	return cmd
}

// promptAliasAfterAdd is the post-`repo add` auto-suggest helper. Opens a
// transient DB handle, asks the user if they want to alias the freshly
// registered repo, and writes the binding (solov2-7w1t). Best-effort —
// errors are logged and swallowed so a prompt failure never breaks the
// add flow itself.
func promptAliasAfterAdd(ctx context.Context, w io.Writer, repoID, canonicalURL, rootPath string) {
	db, closeFn, err := openLocalDB()
	if err != nil {
		return
	}
	defer closeFn()
	if err := runAliasSuggestPrompt(ctx, db, repoID, canonicalURL, rootPath, defaultPromptDeps(w)); err != nil {
		fmt.Fprintf(w, "alias prompt: %v\n", err)
	}
}

// looksLikeRepoURL reports whether arg should be treated as a remote git URL
// by `veska repo add`. A literal filesystem path starting with '/', './',
// '~/' or an existing path on disk is always treated as a path; anything
// else that parses cleanly via repo.CanonicalURL is a URL.
func looksLikeRepoURL(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || strings.HasPrefix(arg, "~") {
		return false
	}
	if _, err := os.Stat(arg); err == nil {
		return false
	}
	_, err := repo.CanonicalURL(arg)
	return err == nil
}

// runRepoAddURL implements `veska repo add <url>`: canonicalise the URL,
// short-circuit on a matching canonical_url row, clone to the tracked
// tier with live progress, then register via the daemon (with direct
// fallback) and stamp canonical_url on the new row (solov2-kxo5.3).
//
// Errors during register-or-canonical_url-update roll the clone back so
// a retry starts clean and no orphan directory pretends to be a repo.
func runRepoAddURL(ctx context.Context, cmd *cobra.Command, rawURL string, wait bool) error {
	w := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	canonical, err := repo.CanonicalURL(rawURL)
	if err != nil {
		return fmt.Errorf("repo add: %w", err)
	}

	// Short-circuit if the URL is already registered. Open + close the
	// DB handle for this read so we release the WAL lock before the
	// network-bound clone.
	if db, closeFn, err := openLocalDB(); err == nil {
		existing, ok, lookupErr := repo.LookupByCanonicalURL(ctx, db, canonical)
		closeFn()
		if lookupErr != nil {
			return fmt.Errorf("repo add: %w", lookupErr)
		}
		if ok {
			fmt.Fprintf(w, "repo already registered: %s (%s)\n", shortRepoID(existing.RepoID), existing.RootPath)
			return nil
		}
	}

	dest := repo.TrackedClonePath(config.DefaultVectorDir(), canonical)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("repo add: %w", err)
	}
	// Clear a stale fragment from a prior failed clone so this run
	// doesn't trip over an already-exists destination.
	_ = os.RemoveAll(dest)

	fmt.Fprintf(w, "cloning %s\n", canonical)
	if _, err := repo.Clone(ctx, canonical, dest, stderr); err != nil {
		_ = os.RemoveAll(dest)
		return fmt.Errorf("repo add: %w", err)
	}

	// Register via daemon when up; fall back to direct write. Mirrors
	// the path-based branch below so the post-clone UX is identical.
	id, existed, dialErr := dialAddRepo(ctx, dest)
	via := "via daemon"
	if dialErr != nil {
		if wait {
			_ = os.RemoveAll(dest)
			return fmt.Errorf("repo add --wait: daemon unreachable (%v); start it with `veska service start` and re-run, or drop --wait", dialErr)
		}
		db, closeFn, err := openLocalDB()
		if err != nil {
			_ = os.RemoveAll(dest)
			return fmt.Errorf("repo add: %w", err)
		}
		var addErr error
		id, existed, addErr = repo.Add(ctx, db, dest)
		closeFn()
		if addErr != nil {
			_ = os.RemoveAll(dest)
			return fmt.Errorf("repo add: %w", addErr)
		}
		via = fmt.Sprintf("direct write; daemon dial failed: %v", dialErr)
	}

	// Stamp canonical_url on the freshly-registered row. Failure here
	// rolls back both the row and the clone — a row without canonical_url
	// looks like a normal path-registered repo and would confuse the
	// alias-resolution path that kxo5.6 builds on top of this column.
	db, closeFn, err := openLocalDB()
	if err != nil {
		_ = os.RemoveAll(dest)
		return fmt.Errorf("repo add: %w", err)
	}
	defer closeFn()
	if err := repo.SetCanonicalURL(ctx, db, id, canonical); err != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM repos WHERE repo_id = ?`, id)
		_ = os.RemoveAll(dest)
		return fmt.Errorf("repo add: %w", err)
	}

	verb := "added"
	if existed {
		verb = "already registered"
	}
	fmt.Fprintf(w, "%s repo %s (%s)\n", verb, shortRepoID(id), via)

	if !existed && !wait {
		// db is already open here; reuse it instead of round-tripping
		// openLocalDB. The prompt is TTY-only and swallows its own errors.
		if err := runAliasSuggestPrompt(ctx, db, id, canonical, dest, defaultPromptDeps(w)); err != nil {
			fmt.Fprintf(w, "alias prompt: %v\n", err)
		}
	}

	if dialErr == nil && wait {
		return waitForScanComplete(ctx, w, id)
	}
	if dialErr == nil && !existed {
		logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
		fmt.Fprintln(w, coldScanRunningHint(dest, logPath))
	}
	return nil
}

// tailScanFailureReason scans the tail of daemon.log for the most recent
// ERROR/WARN line referencing repoID and returns a short reason string
// suitable for inline display. Best-effort: returns "" when no matching
// line is found, the log is unreadable, or the JSONL line cannot be parsed.
// Only the last ~64 KiB of the log is inspected to keep the wait loop snappy.
func tailScanFailureReason(logPath, repoID string) string {
	const tailBytes = 64 * 1024
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := int64(0)
	if info.Size() > tailBytes {
		offset = info.Size() - tailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lastReason string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, repoID) {
			continue
		}
		if !strings.Contains(line, `"level":"ERROR"`) && !strings.Contains(line, `"level":"WARN"`) {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
			Err string `json:"err"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		switch {
		case rec.Err != "" && rec.Msg != "":
			lastReason = rec.Msg + ": " + rec.Err
		case rec.Err != "":
			lastReason = rec.Err
		case rec.Msg != "":
			lastReason = rec.Msg
		}
	}
	return lastReason
}

// tailScanCompleteFiles scans the daemon log for the most-recent
// "cold scan: complete" entry for repoID and returns its files_saved
// count. Used as a last-resort source when the scan finished too fast
// for the CLI's poll loop to observe a non-zero FilesSeen (solov2-a17i).
func tailScanCompleteFiles(logPath, repoID string) int {
	const tailBytes = 64 * 1024
	f, err := os.Open(logPath)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	offset := int64(0)
	if info.Size() > tailBytes {
		offset = info.Size() - tailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lastFiles int
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, repoID) || !strings.Contains(line, "cold scan: complete") {
			continue
		}
		var rec struct {
			FilesSaved int `json:"files_saved"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec.FilesSaved > 0 {
			lastFiles = rec.FilesSaved
		}
	}
	return lastFiles
}

// waitForScanComplete polls eng_get_status until the named repo's scan
// has left scans_in_flight, printing one progress line per phase change
// or files-seen jump so the user has a continuous signal instead of a
// silent background scan.
func waitForScanComplete(ctx context.Context, w io.Writer, repoID string) error {
	// solov2-en47: when the scanner sits on a slow file the phase + files_seen
	// don't change, so the original loop printed nothing for tens of seconds.
	// Heartbeat every 10s with the elapsed-since-last-update so the user can
	// see we're still working — and so they can correlate the stall with a
	// specific file via `~/.veska/logs/daemon.log`.
	const heartbeatEvery = 10 * time.Second
	// solov2-a17i: poll at 100ms (was 500ms). Small repos walk + promote in
	// well under 500ms; the old cadence usually missed every intermediate
	// files_seen update and the user saw "walking → 0 files" then "✓ complete"
	// with no count. 100ms gives 5x more chances to catch a non-zero count
	// without meaningfully increasing daemon load (eng_get_status is cheap).
	const pollInterval = 100 * time.Millisecond
	// startupGrace bounds how long --wait keeps polling while the scan has
	// not yet appeared in scans_in_flight. Without it the very first poll
	// can fire before the daemon's scheduler enqueues the scan: the repo
	// looks "not in flight, not promoted" and we'd surface a misleading
	// "scan no longer in flight" failure for what is actually the first
	// repo a junior eng ever adds (solov2-beda). The grace is generous
	// because the daemon may also be cold-starting embedder election.
	const startupGrace = 5 * time.Second
	start := time.Now()
	var lastPhase string
	var lastFiles int
	var maxFiles int
	var sawInFlight bool
	lastEvent := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress := fetchScanProgress(ctx)
		row, inFlight := progress[repoID]
		if inFlight {
			sawInFlight = true
		}
		if !inFlight {
			// Either the scan finished (no entry in scans_in_flight) or the
			// daemon went away. Distinguish by checking the repo's status —
			// promoted means done.
			type listResult struct {
				Repos []repoView `json:"repos"`
			}
			var lr listResult
			if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err == nil {
				for _, r := range lr.Repos {
					if r.RepoID == repoID && r.LastPromotedSHA != "" {
						// solov2-a17i: scan may have finished before our
						// first poll caught a non-zero FilesSeen. Fall back
						// to the daemon log's final files_saved count so
						// "✓ complete" carries the file count we actually
						// indexed.
						files := maxFiles
						if files == 0 {
							logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
							files = tailScanCompleteFiles(logPath, repoID)
						}
						if files > 0 {
							plural := "s"
							if files == 1 {
								plural = ""
							}
							fmt.Fprintf(w, "  ✓ cold scan complete: %d file%s (%.1fs)\n", files, plural, time.Since(start).Seconds())
						} else {
							fmt.Fprintf(w, "  ✓ cold scan complete (%.1fs)\n", time.Since(start).Seconds())
						}
						return nil
					}
				}
			}
			// Not promoted and no in-flight entry. Two distinct cases:
			//
			//  (a) The scan never started yet — daemon scheduler hasn't
			//      enqueued it. Stay in the loop until startupGrace elapses
			//      so we don't surface a false-negative on the user's very
			//      first repo (solov2-beda). After grace, treat it as a
			//      scheduler issue and error out.
			//  (b) The scan was previously in-flight and has since left
			//      the set without producing a last_promoted_sha — that's
			//      a real failure; surface the daemon log's cause.
			logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
			if !sawInFlight && time.Since(start) < startupGrace {
				time.Sleep(pollInterval)
				continue
			}
			if reason := tailScanFailureReason(logPath, repoID); reason != "" {
				fmt.Fprintf(w, "  ✗ cold scan failed: %s\n", reason)
				fmt.Fprintf(w, "    full context: tail %s\n", logPath)
				return fmt.Errorf("cold scan failed")
			}
			if !sawInFlight {
				fmt.Fprintf(w, "  ✗ scan never started after %.0fs — daemon may be wedged; tail %s\n", time.Since(start).Seconds(), logPath)
				return fmt.Errorf("cold scan did not start")
			}
			fmt.Fprintf(w, "  scan no longer in flight, repo not yet promoted — tail %s for the cause\n", logPath)
			return nil
		}
		if row.FilesSeen > maxFiles {
			maxFiles = row.FilesSeen
		}
		// solov2-a17i: suppress the "0 files (0.0s)" first tick — it just
		// reflects the race between scan start and the first poll and
		// reads as broken. We still report when files_seen first crosses
		// 0 (real progress) or when phase changes after we've seen files.
		if row.Phase != lastPhase || row.FilesSeen != lastFiles {
			meaningful := row.FilesSeen > 0 || (lastPhase != "" && row.Phase != lastPhase)
			if meaningful {
				phase := row.Phase
				if phase == "" {
					phase = "running"
				}
				fmt.Fprintf(w, "  %s → %d files (%.1fs)\n", phase, row.FilesSeen, time.Since(start).Seconds())
				lastEvent = time.Now()
			}
			lastPhase = row.Phase
			lastFiles = row.FilesSeen
		} else if time.Since(lastEvent) >= heartbeatEvery {
			phase := row.Phase
			if phase == "" {
				phase = "running"
			}
			fmt.Fprintf(w, "  %s → %d files (%.1fs, stalled %.0fs — check ~/.veska/logs/daemon.log)\n",
				phase, row.FilesSeen, time.Since(start).Seconds(), time.Since(lastEvent).Seconds())
			lastEvent = time.Now()
		}
		time.Sleep(pollInterval)
	}
}

// repoRemoveCmd unifies the deregister surface (solov2-meuk):
//   - `repo remove <id|path>` — remove one (original behavior)
//   - `repo remove --missing`  — remove every repo whose root dir is gone
//     (the old `repo prune` behavior)
//   - `repo remove --all`      — wipe registry (requires --yes confirmation)
//   - `--dry-run` is honored for --missing and --all.
//
// `repo prune` remains as a hidden alias for one release; see repoPruneCmd.
func repoRemoveCmd() *cobra.Command {
	var (
		missing bool
		all     bool
		yes     bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:          "remove [<id-or-path>]",
		Short:        "Deregister a repository and remove hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case missing && all:
				return fmt.Errorf("repo remove: --missing and --all are mutually exclusive")
			case (missing || all) && len(args) == 1:
				return fmt.Errorf("repo remove: positional argument not allowed with --missing/--all")
			case !missing && !all && len(args) == 0:
				return fmt.Errorf("repo remove: missing repo id-or-path (or pass --missing / --all)")
			}
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			if missing {
				return removeMissingRepos(ctx, w, dryRun)
			}
			if all {
				return removeAllRepos(ctx, w, cmd.InOrStdin(), dryRun, yes)
			}
			return removeOneRepo(ctx, w, args[0])
		},
	}
	cmd.Flags().BoolVar(&missing, "missing", false, "remove every repo whose root directory no longer exists")
	cmd.Flags().BoolVar(&all, "all", false, "remove every registered repo (requires --yes or interactive confirmation)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip interactive confirmation (required for --all in scripts)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be removed without changing the registry")
	return cmd
}

// removeOneRepo is the original single-repo deregister path.
func removeOneRepo(ctx context.Context, w io.Writer, arg string) error {
	// solov2-jtl5.2: accept the same identifiers `repo add` does.
	id, resolveErr := resolveRepoArg(ctx, arg)
	if resolveErr != nil {
		return fmt.Errorf("repo remove: %w", resolveErr)
	}
	if err := dialRemoveRepo(ctx, id); err == nil {
		fmt.Fprintln(w, "removed (via daemon)")
		return nil
	}
	db, closeFn, err := openLocalDB()
	if err != nil {
		return fmt.Errorf("repo remove: %w", err)
	}
	defer closeFn()
	if err := repo.Remove(ctx, db, id); err != nil {
		return fmt.Errorf("repo remove: %w", err)
	}
	fmt.Fprintln(w, "removed (direct write; daemon offline)")
	return nil
}

// listRegisteredRepos returns the repo set via the daemon when up, falling
// back to direct DB read. Shared by removeMissingRepos / removeAllRepos so
// the unified verb sees the same registry the legacy prune did.
func listRegisteredRepos(ctx context.Context) ([]repoView, error) {
	type listResult struct {
		Repos []repoView `json:"repos"`
	}
	var lr listResult
	if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err == nil {
		return lr.Repos, nil
	}
	db, closeFn, err := openLocalDB()
	if err != nil {
		return nil, err
	}
	defer closeFn()
	recs, err := repo.List(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]repoView, 0, len(recs))
	for _, r := range recs {
		out = append(out, repoView{
			RepoID: r.RepoID, RootPath: r.RootPath,
			ActiveBranch: r.ActiveBranch, LastPromotedSHA: r.LastPromotedSHA,
			Kind: r.Kind, Aliases: r.Aliases,
		})
	}
	return out, nil
}

// removeMissingRepos is the old `repo prune` body, lifted under the unified
// verb. Daemon-up uses dialRemoveRepo; daemon-down uses direct repo.Remove.
func removeMissingRepos(ctx context.Context, w io.Writer, dryRun bool) error {
	repos, err := listRegisteredRepos(ctx)
	if err != nil {
		return fmt.Errorf("repo remove --missing: %w", err)
	}
	var missing []repoView
	for _, r := range repos {
		if _, statErr := os.Stat(r.RootPath); os.IsNotExist(statErr) {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(w, "no missing repos — nothing to remove")
		return nil
	}
	return applyBulkRemove(ctx, w, missing, dryRun, "missing")
}

// removeAllRepos wipes the whole registry. Requires --yes when non-interactive.
func removeAllRepos(ctx context.Context, w io.Writer, in io.Reader, dryRun, yes bool) error {
	repos, err := listRegisteredRepos(ctx)
	if err != nil {
		return fmt.Errorf("repo remove --all: %w", err)
	}
	if len(repos) == 0 {
		fmt.Fprintln(w, "registry is empty — nothing to remove")
		return nil
	}
	if !dryRun && !yes {
		fmt.Fprintf(w, "about to remove all %d registered repo(s). Continue? [y/N] ", len(repos))
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Fprintln(w, "aborted")
			return nil
		}
	}
	return applyBulkRemove(ctx, w, repos, dryRun, "all")
}

// applyBulkRemove iterates targets and removes each via daemon-or-direct,
// printing per-row status and a trailing summary. Errors on individual rows
// are printed but do not abort the loop — partial cleanup is better than
// none.
func applyBulkRemove(ctx context.Context, w io.Writer, targets []repoView, dryRun bool, scope string) error {
	useDaemon := false
	if _, err := dialEngStatus(ctx); err == nil {
		useDaemon = true
	}
	var db *sql.DB
	var closeFn func()
	if !useDaemon {
		var err error
		db, closeFn, err = openLocalDB()
		if err != nil {
			return fmt.Errorf("repo remove --%s: %w", scope, err)
		}
		defer closeFn()
	}
	for _, r := range targets {
		prefix := "would remove"
		if !dryRun {
			prefix = "removing"
		}
		fmt.Fprintf(w, "%s %s  %s\n", prefix, shortRepoID(r.RepoID), r.RootPath)
		if dryRun {
			continue
		}
		var rmErr error
		if useDaemon {
			rmErr = dialRemoveRepo(ctx, r.RepoID)
		} else {
			rmErr = repo.Remove(ctx, db, r.RepoID)
		}
		if rmErr != nil {
			fmt.Fprintf(w, "  failed: %v\n", rmErr)
		}
	}
	if dryRun {
		fmt.Fprintf(w, "%d candidate(s) — rerun without --dry-run to apply\n", len(targets))
	} else {
		fmt.Fprintf(w, "removed %d repo(s)\n", len(targets))
	}
	return nil
}

// dialEngStatus probes whether the daemon socket is reachable. Used by
// applyBulkRemove to choose the daemon path vs direct DB without a separate
// guess.
func dialEngStatus(ctx context.Context) (any, error) {
	var resp any
	err := callMCP(ctx, "eng_get_status", map[string]any{}, &resp)
	return resp, err
}

// resolveRepoArg returns the canonical repo_id for arg. A hex-only string
// (repo_id or short_id prefix) is returned unchanged — the registry already
// resolves prefixes. Anything else is treated as a filesystem path: it is
// resolved to absolute form and matched against the RootPath of every
// registered repo. The not-found error mentions the resolved abs path so
// the user sees what we actually looked up.
func resolveRepoArg(ctx context.Context, arg string) (string, error) {
	if looksLikeRepoID(arg) {
		return arg, nil
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", arg, err)
	}
	db, closeFn, err := openLocalDB()
	if err != nil {
		return "", err
	}
	defer closeFn()
	records, err := repo.List(ctx, db)
	if err != nil {
		return "", fmt.Errorf("list registered repos: %w", err)
	}
	for _, r := range records {
		if r.RootPath == abs {
			return r.RepoID, nil
		}
	}
	return "", fmt.Errorf("no registered repo with root %q (use `veska repo list` to see registered repos)", abs)
}

// looksLikeRepoID reports whether arg is plausibly a repo_id or short_id
// prefix — a non-empty hex-only string. Repo IDs are SHA-256 hex; even a
// 4-char prefix is uniquely identifying in practice. Filesystem paths almost
// always contain a non-hex character (`/`, `.`, `-`).
func looksLikeRepoID(arg string) bool {
	if arg == "" {
		return false
	}
	for _, c := range arg {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// openLocalDB opens the on-disk sqlite database with full migrations applied
// and returns a close function so the caller releases the WAL connection
// promptly. Used as the fallback path when the daemon is not running.
func openLocalDB() (*sql.DB, func(), error) {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	handle, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	return handle, func() { _ = handle.Close() }, nil
}

// dialAddRepo sends eng_add_repo over the daemon's MCP unix socket. Returns
// the assigned repo_id on success. Errors are translated into context — a
// dial failure means "daemon not running" and the caller should fall back.
func dialAddRepo(ctx context.Context, rootPath string) (string, bool, error) {
	type result struct {
		RepoID            string `json:"repo_id"`
		ScanPending       bool   `json:"scan_pending"`
		AlreadyRegistered bool   `json:"already_registered"`
	}
	var r result
	if err := callMCP(ctx, "eng_add_repo", map[string]any{"root_path": rootPath}, &r); err != nil {
		return "", false, err
	}
	if r.RepoID == "" {
		return "", false, errors.New("daemon returned empty repo_id")
	}
	return r.RepoID, r.AlreadyRegistered, nil
}

// dialRemoveRepo sends eng_remove_repo over the daemon's MCP unix socket.
func dialRemoveRepo(ctx context.Context, repoID string) error {
	var r struct{}
	return callMCP(ctx, "eng_remove_repo", map[string]any{"repo_id": repoID}, &r)
}

// callMCP performs a single newline-delimited JSON-RPC call against the
// daemon's MCP socket and decodes result into out. Returns an error if the
// dial fails (daemon not running), if the response is an error frame, or
// if decoding fails.
//
// The MCP server here speaks a direct flat protocol (method = tool name),
// not the standard MCP "tools/call" wrapper — see internal/infrastructure/mcp.
//
// Dialing retries 3× with 200ms backoff: a daemon restart binds the
// socket in two steps (listenUnix removes the stale path, then net.Listen
// creates a new one), and a CLI call racing that window used to fall
// straight through to the direct-write path even with the daemon up
// (solov2-0cg). 2s per-attempt + 3 attempts = ~6s ceiling, still well
// under any human-perceptible wait.

// methodsSkipCwd lists eng_* methods that must NOT receive an auto-
// injected cwd. These are tools whose CLI surface intentionally fans
// out across every registered repo when --repo is omitted (solov2-efzv):
// auto-injecting cwd would silently pin them to a single repo and
// break the multi-repo workflow on the cobra-CLI-plus-shared-lib
// pattern.
var methodsSkipCwd = map[string]struct{}{
	"eng_find_symbol":      {},
	"eng_get_context_pack": {},
}

// withCwdInjected adds a "cwd" field to params for eng_* methods so the
// daemon can resolve repo_id from the caller's working directory when
// omitted (solov2-ktz0). Non-eng_* methods, frames that already carry
// cwd, and methods in methodsSkipCwd pass through unchanged.
func withCwdInjected(method string, params any) any {
	if !strings.HasPrefix(method, "eng_") {
		return params
	}
	if _, skip := methodsSkipCwd[method]; skip {
		return params
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return params
	}
	if params == nil {
		return map[string]any{"cwd": cwd}
	}
	m, ok := params.(map[string]any)
	if !ok {
		return params
	}
	if existing, _ := m["cwd"].(string); existing != "" {
		return m
	}
	m["cwd"] = cwd
	return m
}

// humanizeMCPError rewrites MCP-protocol-flavored hints into CLI-flavored
// ones so users running `veska` don't see references to `eng_*` tool names
// or JSON-RPC error codes they have no way to act on (solov2-luc7).
func humanizeMCPError(msg string) string {
	rep := strings.NewReplacer(
		"pass eng_list_repos to find the id", "run `veska repo list` to see ids",
		"run eng_list_repos", "run `veska repo list`",
	)
	return rep.Replace(msg)
}

func callMCP(ctx context.Context, method string, params any, out any) error {
	const dialTimeout = 2 * time.Second
	const dialBackoff = 200 * time.Millisecond
	const dialAttempts = 3
	// solov2-d37i: the first call after `veska service start` (cold daemon)
	// can take ~10s as SQLite opens, the embedder hot-loads, and registries
	// fully initialise. The previous 5s ceiling occasionally tripped on
	// eng_add_repo and the CLI fell through to the direct-write path with
	// a confusing 'restart daemon' hint. 30s is well within human patience
	// for a one-shot CLI call and absorbs the cold-start jitter.
	const ioTimeout = 30 * time.Second

	// solov2-7x7l: dial cli.sock, not mcp.sock. The daemon classifies the
	// actor based on which socket the connection lands on (server.go:104/108):
	// cli.sock → ActorKindHuman, mcp.sock → ActorKindAgent. Routing CLI
	// commands through mcp.sock caused every `veska findings close` of a
	// high-severity finding to fail human_required, even though the user
	// was on the actual CLI. mcp.sock stays reserved for editor-driven
	// veska-mcp shim clients.
	sockPath := config.CLISockPath()
	var (
		conn    net.Conn
		dialErr error
		d       net.Dialer
	)
	d.Timeout = dialTimeout
	for attempt := range dialAttempts {
		conn, dialErr = d.DialContext(ctx, "unix", sockPath)
		if dialErr == nil {
			break
		}
		if attempt < dialAttempts-1 {
			time.Sleep(dialBackoff)
		}
	}
	if dialErr != nil {
		// Include the underlying dial error so 'daemon offline' messages
		// can tell the user what really happened (connection refused vs.
		// no such file vs. permission denied — solov2-0cg). When the cause
		// is "daemon not running", append an actionable hint pointing at
		// `veska service start` (solov2-j68l).
		es := dialErr.Error()
		if strings.Contains(es, "connection refused") ||
			strings.Contains(es, "no such file") ||
			strings.Contains(es, "no such file or directory") {
			return fmt.Errorf("dial %s: daemon not running (start it with `veska service start`, or run `veska-daemon &` for a quick try): %w", sockPath, dialErr)
		}
		return fmt.Errorf("dial %s after %d attempts: %w", sockPath, dialAttempts, dialErr)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))

	type req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	// solov2-ktz0 / solov2-zukc: inject cwd into eng_* params so the daemon
	// can fall back to it when repo_id is omitted. Mirrors what veska-mcp
	// does for editor-driven clients (cmd/veska-mcp/cwd_inject.go) — without
	// this, CLI wrappers (veska symbol, veska context, …) would still have
	// to plumb cwd themselves.
	params = withCwdInjected(method, params)
	body, err := json.Marshal(req{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	scanner := bufio.NewScanner(conn)
	// Allow large embedded results (e.g. find_symbol with many hits).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read response: %w", err)
		}
		return errors.New("no response from daemon")
	}

	type rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	type resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *rpcErr         `json:"error,omitempty"`
	}
	var r resp
	if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("daemon: %s", humanizeMCPError(r.Error.Message))
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}
