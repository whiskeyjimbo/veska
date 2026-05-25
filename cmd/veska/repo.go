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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
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
	return cmd
}

// repoPruneCmd removes registered repos whose root directory is gone — a
// recurring state when checkouts move or get cleaned up. Without prune,
// the daemon logs a WARN every boot for each missing repo (solov2-s0t0).
// --dry-run lists the candidates without removing them (solov2-47yj).
func repoPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Remove registered repos whose root directory no longer exists",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()

			type listResult struct {
				Repos []repoView `json:"repos"`
			}
			var lr listResult
			useDaemon := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr) == nil
			var repos []repoView
			var db *sql.DB
			var closeFn func()
			if !useDaemon {
				var err error
				db, closeFn, err = openLocalDB()
				if err != nil {
					return fmt.Errorf("repo prune: %w", err)
				}
				defer closeFn()
				recs, err := repo.List(ctx, db)
				if err != nil {
					return fmt.Errorf("repo prune: %w", err)
				}
				for _, r := range recs {
					repos = append(repos, repoView{
						RepoID: r.RepoID, RootPath: r.RootPath,
						ActiveBranch: r.ActiveBranch, LastPromotedSHA: r.LastPromotedSHA,
					})
				}
			} else {
				repos = lr.Repos
			}

			var missing []repoView
			for _, r := range repos {
				if _, err := os.Stat(r.RootPath); os.IsNotExist(err) {
					missing = append(missing, r)
				}
			}
			if len(missing) == 0 {
				fmt.Fprintln(w, "no missing repos — nothing to prune")
				return nil
			}
			for _, r := range missing {
				prefix := "would remove"
				if !dryRun {
					prefix = "removing"
				}
				fmt.Fprintf(w, "%s %s  %s\n", prefix, shortRepoID(r.RepoID), r.RootPath)
				if dryRun {
					continue
				}
				if useDaemon {
					if err := dialRemoveRepo(ctx, r.RepoID); err != nil {
						fmt.Fprintf(w, "  failed: %v\n", err)
					}
				} else {
					if err := repo.Remove(ctx, db, r.RepoID); err != nil {
						fmt.Fprintf(w, "  failed: %v\n", err)
					}
				}
			}
			if dryRun {
				fmt.Fprintf(w, "%d candidate(s) — rerun without --dry-run to apply\n", len(missing))
			} else {
				fmt.Fprintf(w, "pruned %d repo(s)\n", len(missing))
			}
			return nil
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
}

// repoListCmd prints every registered repo (solov2-0pq). Prefers the
// running daemon's eng_list_repos so the listing matches what the daemon
// sees (including any in-flight scan state surfaced via degraded_reasons);
// falls back to a direct SQLite read so the CLI still works when the
// daemon is down.
func repoListCmd() *cobra.Command {
	return &cobra.Command{
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
			if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err == nil {
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
				views = append(views, repoView{
					RepoID:          r.RepoID,
					RootPath:        r.RootPath,
					ActiveBranch:    r.ActiveBranch,
					LastPromotedSHA: r.LastPromotedSHA,
				})
			}
			printRepoTable(w, views)
			return nil
		},
	}
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
// exact full id, then 12-char short_id, then unambiguous prefix (>= 4 chars).
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
	return repo.Record{}, fmt.Errorf("repo %q is not registered (prefixes must be >= %d chars)", repoID, cliMinRepoIDPrefix)
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
	fmt.Fprintln(tw, "REPO_ID\tBRANCH\tSTATUS\tROOT")
	for _, r := range repos {
		short := shortRepoID(r.RepoID)
		branch := r.ActiveBranch
		if branch == "" {
			branch = "-"
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", short, branch, status, r.RootPath)
	}
	_ = tw.Flush()
}

func repoAddCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:          "add <path>",
		Short:        "Register a git repository and install hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			root := args[0]

			// Prefer the daemon when up — it triggers cold scan and seeds
			// the live watcher in one call (parity with eng_add_repo).
			id, existed, dialErr := dialAddRepo(ctx, root)
			if dialErr == nil {
				if existed {
					fmt.Fprintf(w, "repo already registered: %s (via daemon)\n", shortRepoID(id))
					return nil
				}
				fmt.Fprintf(w, "added repo %s (via daemon)\n", shortRepoID(id))
				if wait {
					return waitForScanComplete(ctx, w, id)
				}
				logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
				fmt.Fprintf(w, "  cold scan running in the background — `veska repo list` shows status; re-run with `--wait` to block until it finishes, or `tail %s` for live progress\n", logPath)
				return nil
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
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the cold scan completes; print live progress")
	return cmd
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
	start := time.Now()
	var lastPhase string
	var lastFiles int
	lastEvent := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress := fetchScanProgress(ctx)
		row, inFlight := progress[repoID]
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
						fmt.Fprintf(w, "  ✓ cold scan complete (%.1fs)\n", time.Since(start).Seconds())
						return nil
					}
				}
			}
			// Not promoted and no in-flight entry — scan likely failed
			// (or finished before we got our first poll in). Surface the
			// daemon log's most recent error for this repo when we can
			// find one, so the user sees the cause inline instead of
			// being told to grep (solov2-jtl5.7).
			logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
			if reason := tailScanFailureReason(logPath, repoID); reason != "" {
				fmt.Fprintf(w, "  ✗ cold scan failed: %s\n", reason)
				fmt.Fprintf(w, "    full context: tail %s\n", logPath)
				return fmt.Errorf("cold scan failed")
			}
			fmt.Fprintf(w, "  scan no longer in flight, repo not yet promoted — tail %s for the cause\n", logPath)
			return nil
		}
		if row.Phase != lastPhase || row.FilesSeen != lastFiles {
			phase := row.Phase
			if phase == "" {
				phase = "running"
			}
			fmt.Fprintf(w, "  %s → %d files (%.1fs)\n", phase, row.FilesSeen, time.Since(start).Seconds())
			lastPhase = row.Phase
			lastFiles = row.FilesSeen
			lastEvent = time.Now()
		} else if time.Since(lastEvent) >= heartbeatEvery {
			phase := row.Phase
			if phase == "" {
				phase = "running"
			}
			fmt.Fprintf(w, "  %s → %d files (%.1fs, stalled %.0fs — check ~/.veska/logs/daemon.log)\n",
				phase, row.FilesSeen, time.Since(start).Seconds(), time.Since(lastEvent).Seconds())
			lastEvent = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func repoRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "remove <id-or-path>",
		Short:        "Deregister a repository and remove hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			arg := args[0]

			// solov2-jtl5.2: accept the same identifiers `repo add` does.
			// If arg looks like a filesystem path, resolve it to a repo_id
			// via the registry before dialing the daemon. A repo_id (or
			// short_id prefix) is passed through unchanged so existing
			// usage isn't affected.
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
		},
	}
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
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
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

// withCwdInjected adds a "cwd" field to params for eng_* methods so the
// daemon can resolve repo_id from the caller's working directory when
// omitted (solov2-ktz0). Non-eng_* methods and frames that already carry
// cwd pass through unchanged.
func withCwdInjected(method string, params any) any {
	if !strings.HasPrefix(method, "eng_") {
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

	sockPath := config.MCPSockPath()
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
		return fmt.Errorf("daemon: %s (code %d)", r.Error.Message, r.Error.Code)
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}
