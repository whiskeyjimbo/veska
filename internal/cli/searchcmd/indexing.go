package searchcmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/composition"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// ensureIndexed runs a synchronous cold-scan + embedder drain when the repo
// has no promoted SHA yet. Subsequent runs against the same repo short-circuit,
// satisfying AC2 (re-use of the existing index).
// The embedder is started, the pending queue drained, and the worker stopped
// turning the daemon's long-running goroutine into a one-shot pass that fits
// the CLI's lifecycle.
func ensureIndexed(ctx context.Context, pools *sqlite.Pools, rec repo.Record, opts RunOpts, w io.Writer) error {
	if rec.LastPromotedSHA != "" {
		return nil
	}
	fmt.Fprintf(w, "search: indexing %s (first run)...\n", rec.RootPath)

	loader := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := opts.ReparserFactory(pools, loader)
	if err != nil {
		return err
	}
	appRec := application.RepoRecord{
		RepoID:          rec.RepoID,
		RootPath:        rec.RootPath,
		ActiveBranch:    rec.ActiveBranch,
		LastPromotedSHA: rec.LastPromotedSHA,
	}
	if appRec.ActiveBranch == "" {
		appRec.ActiveBranch = "main"
	}
	if err := reparser(ctx, appRec); err != nil {
		return fmt.Errorf("search: cold scan: %w", err)
	}

	// surface the same per-rule check summary that
	// `repo add --wait` prints, so a junior running
	// `veska search <q> --repo <url>` sees what the promotion checks found
	// before the search results scroll past. Silent on a clean promotion.
	emitColdScanSummary(ctx, pools.ReadDB, w, appRec.RepoID, appRec.ActiveBranch)

	return drainEmbedderQueue(ctx, pools, w)
}

// EmitColdScanSummary prints a one-line "✓ <rule>: N finding(s)" entry per
// non-zero rule for the given (repo, branch), matching the per-rule summary
// the `repo add --wait` flow surfaces. Silent when no findings exist so a
// clean promotion doesn't pollute `veska search --repo <url>` output. Best
// effort: any DB error is swallowed - the summary is advisory, not load
// bearing.
func EmitColdScanSummary(ctx context.Context, db *sql.DB, w io.Writer, repoID, branch string) {
	emitColdScanSummary(ctx, db, w, repoID, branch)
}

func emitColdScanSummary(ctx context.Context, db *sql.DB, w io.Writer, repoID, branch string) {
	if db == nil {
		return
	}
	counts, err := sqlite.NewFindingQuerierRepo(db).OpenFindingCountsByRule(ctx, repoID, branch)
	if err != nil {
		return
	}
	rules := make([]string, 0, len(counts))
	for rule := range counts {
		rules = append(rules, rule)
	}
	sort.Strings(rules)
	for _, rule := range rules {
		n := counts[rule]
		if n == 0 {
			continue
		}
		plural := "s"
		if n == 1 {
			plural = ""
		}
		fmt.Fprintf(w, "  ✓ %s: %d finding%s\n", rule, n, plural)
	}
}

// drainEmbedderQueue starts an Embedder.Worker, polls the pending-refs count
// to zero (or a deadline), then stops the worker. The CLI must not return
// before vectors are populated - otherwise the search runs against an empty
// vector index and returns no hits.
func drainEmbedderQueue(ctx context.Context, pools *sqlite.Pools, w io.Writer) error {
	worker, refs, err := buildDrainWorker(pools)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	worker.Start(wctx)
	defer func() {
		cancel()
		worker.Wait()
	}()

	return pollEmbedderDrain(ctx, refs, w)
}

// buildDrainWorker wires the one-shot embedder worker (provider + vector
// storage + refs repo) used to drain the pending queue.
func buildDrainWorker(pools *sqlite.Pools) (*embedder.Worker, *sqlite.EmbeddingRefsRepo, error) {
	prov, err := composition.NewCLIEmbeddingProvider()
	if err != nil {
		return nil, nil, err
	}
	vec, err := vector.NewVectorStorage(vector.BackendMemory, config.DefaultVectorDir())
	if err != nil {
		return nil, nil, fmt.Errorf("search: open vector storage: %w", err)
	}
	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write)
	worker, err := embedder.NewWorker(refs, prov, vec,
		embedder.WithInterval(100*time.Millisecond),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("search: build embedder worker: %w", err)
	}
	return worker, refs, nil
}

// pollEmbedderDrain polls the pending-refs count until empty or a generous
// wall clock. The default per-tick interval is 100ms so a few thousand pending
// rows take a few seconds. A 10-minute ceiling keeps a stuck embedder (Ollama
// unreachable mid-drain) from wedging the CLI.
func pollEmbedderDrain(ctx context.Context, refs *sqlite.EmbeddingRefsRepo, w io.Writer) error {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pending, err := refs.CountPending(ctx)
		if err != nil {
			return fmt.Errorf("search: count pending: %w", err)
		}
		if pending == 0 {
			fmt.Fprintln(w, "search: index ready")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("search: embedder drain timeout, %d pending rows remain", pending)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// EphemeralEnsureFromURL implements the URL-target half of `veska search`
// Steps:
//  1. canonicalise the URL
//  2. consult canonical_url for an existing row (tracked or ephemeral) - if
//     hit, reuse it (and bump last_accessed_at when ephemeral); no clone. AC3:
//     ephemeral hit whose cache dir vanished triggers a silent re-clone
//     instead of erroring.
//  3. otherwise clone --depth=1 into RepoCachePath, repo.Add, flip kind to
//     'ephemeral', stamp canonical_url, touch last_accessed_at
//
// Multi-phase progress (AC2) is rendered inline: the "cloning <url>" banner
// here, git's --progress lines via the Clone helper, the existing "search:
// indexing …" line from ensureIndexed, and the "search: index ready" line from
// drainEmbedderQueue.
func EphemeralEnsureFromURL(ctx context.Context, pools *sqlite.Pools, rawURL string, w io.Writer) (repo.Record, error) {
	return ephemeralEnsureFromURL(ctx, pools, rawURL, w)
}

func ephemeralEnsureFromURL(ctx context.Context, pools *sqlite.Pools, rawURL string, w io.Writer) (repo.Record, error) {
	canonical, err := repo.CanonicalURL(rawURL)
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: %w", err)
	}

	existing, ok, err := repo.LookupByCanonicalURL(ctx, pools.ReadDB, canonical)
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: %w", err)
	}
	if ok {
		if rec, reused := reuseExistingClone(ctx, pools, existing, canonical, w); reused {
			return rec, nil
		}
	}
	return cloneEphemeral(ctx, pools, canonical, w)
}

// reuseExistingClone short-circuits the clone when a canonical_url match
// exists. reused is false only for an ephemeral row whose cache dir vanished
// the caller then re-clones (AC3); the stale row is dropped here first so the
// re-add doesn't trip the UNIQUE(root_path) index.
func reuseExistingClone(ctx context.Context, pools *sqlite.Pools, existing repo.Record, canonical string, w io.Writer) (repo.Record, bool) {
	if existing.Kind != "ephemeral" {
		// tracked match - the URL points at code the user already has
		// locally registered. Skip the clone entirely; the existing index
		// is authoritative.
		fmt.Fprintf(w, "search: %s is already tracked at %s\n", canonical, existing.RootPath)
		return existing, true
	}
	if _, statErr := os.Stat(existing.RootPath); statErr != nil {
		// AC3: cache dir vanished (user wiped ~/.cache, manual rm, etc.) →
		// re-clone silently. Drop the stale row first.
		_, _ = pools.Write.ExecContext(ctx,
			`DELETE FROM repos WHERE repo_id = ?`, existing.RepoID)
		return repo.Record{}, false
	}
	_ = repo.TouchEphemeral(ctx, pools.Write, existing.RepoID)
	fmt.Fprintf(w, "search: reusing cached clone at %s\n", existing.RootPath)
	return existing, true
}

// cloneEphemeral performs the depth-1 clone into the cache tier, registers it,
// flips kind to 'ephemeral', stamps canonical_url, and touches
// last_accessed_at.
func cloneEphemeral(ctx context.Context, pools *sqlite.Pools, canonical string, w io.Writer) (repo.Record, error) {
	dest := config.RepoCachePath(repo.DerivedRepoIDFromURL(canonical))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return repo.Record{}, fmt.Errorf("search: mkdir cache: %w", err)
	}
	_ = os.RemoveAll(dest)

	fmt.Fprintf(w, "cloning %s → %s\n", canonical, dest)
	if _, err := repo.Clone(ctx, canonical, dest, w); err != nil {
		_ = os.RemoveAll(dest)
		return repo.Record{}, fmt.Errorf("search: %w", err)
	}

	id, _, err := repo.Add(ctx, pools.Write, dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return repo.Record{}, fmt.Errorf("search: register cloned repo: %w", err)
	}
	if _, err := pools.Write.ExecContext(ctx,
		`UPDATE repos SET kind = 'ephemeral' WHERE repo_id = ?`, id); err != nil {
		return repo.Record{}, fmt.Errorf("search: mark ephemeral: %w", err)
	}
	if err := repo.SetCanonicalURL(ctx, pools.Write, id, canonical); err != nil {
		return repo.Record{}, fmt.Errorf("search: stamp canonical_url: %w", err)
	}
	_ = repo.TouchEphemeral(ctx, pools.Write, id)

	rec, err := repo.Get(ctx, pools.ReadDB, id)
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: load registered repo: %w", err)
	}
	if rec.ActiveBranch == "" {
		rec.ActiveBranch = "main"
	}
	return rec, nil
}

// IsGitURL reports whether s should be treated as a remote git URL by the
// search command. Strings starting with a path prefix or matching an existing
// filesystem path are always paths; anything else that parses via
// repo.CanonicalURL is a URL. Mirrors looksLikeRepoURL in repo.go so `veska
// search` and `veska repo add` agree on what a URL is.
func IsGitURL(s string) bool { return isGitURL(s) }

func isGitURL(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "~") {
		return false
	}
	if _, err := os.Stat(s); err == nil {
		return false
	}
	_, err := repo.CanonicalURL(s)
	return err == nil
}
