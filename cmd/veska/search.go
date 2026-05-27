package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// searchCmd is the one-shot eval CLI from solov2-z92: clone+index+query
// in a single command, no daemon required. It is a thin wrapper around
// the in-process services the daemon also wires — Ingester, Promoter,
// EmbedWorker, VectorStorage, search.Service — bolted together for a
// synchronous one-pass run instead of long-lived goroutines.
//
//	veska search "<query>"                       # search existing index
//	veska search "<query>" <path>                # ensure indexed, then search
//	veska search "<query>" https://github.com/x  # clone, index, search
func searchCmd() *cobra.Command {
	var (
		k        int
		jsonOut  bool
		repoFlag string
	)
	cmd := &cobra.Command{
		Use:   "search <query> [path-or-url]",
		Short: "Semantic search; optionally clone+index a repo first",
		Long: `Semantic search against an indexed repo.

The optional second argument (or --repo flag) selects the repo to search:
  - omitted        — auto-detect from cwd (must be a registered repo)
  - local path     — registered local repo (absolute or relative)
  - git URL        — clones into the cache tier (~/.cache/veska/repos/<id>),
                     marks as ephemeral, indexes it, then searches

Examples:
  veska search "parse config"                            # search the repo containing cwd
  veska search "parse config" /path/to/myrepo            # search a specific registered local repo
  veska search "parse config" --repo https://github.com/x  # clone (ephemeral), index, search
`,
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			var target string
			if len(args) == 2 {
				target = args[1]
			}
			if repoFlag != "" {
				if target != "" && target != repoFlag {
					return fmt.Errorf("search: --repo and the positional target both set to different values")
				}
				target = repoFlag
			}
			return runSearch(cmd.Context(), cmd.OutOrStdout(), runSearchOpts{
				query:   query,
				target:  target,
				k:       k,
				jsonOut: jsonOut,
			})
		},
	}
	cmd.Flags().IntVarP(&k, "limit", "k", 10, "max results to return")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (same shape as eng_search_semantic)")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo target (path, URL, repo_id or short_id) — alias for the positional argument (solov2-kxo5.6)")
	return cmd
}

type runSearchOpts struct {
	query   string
	target  string
	k       int
	jsonOut bool
}

func runSearch(ctx context.Context, w io.Writer, opts runSearchOpts) error {
	// Daemon-first: when a daemon is up and already tracks the target repo,
	// run the query through its eng_search_semantic so the CLI shares the
	// daemon's hybrid (vector + lexical) retrieval pipeline and never opens
	// a second writer on veska.db (solov2-b1q, solov2-xkm). The in-process
	// path below is the fallback for when the daemon is down or the repo is
	// not yet registered (it clones/indexes synchronously).
	if env, handled, err := daemonSearch(ctx, opts); handled {
		if err != nil {
			return err
		}
		return renderSearchEnvelope(w, env, opts.jsonOut)
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		return fmt.Errorf("search: migrate sqlite: %w", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("search: open sqlite pools: %w", err)
	}
	defer func() { _ = pools.Close() }()

	rec, err := resolveSearchTarget(ctx, pools, opts.target, w)
	if err != nil {
		return err
	}

	if err := ensureIndexed(ctx, pools, rec, w); err != nil {
		return err
	}

	svc, err := buildSearchService(pools)
	if err != nil {
		return err
	}

	resp, err := svc.Semantic(ctx, rec.RepoID, rec.ActiveBranch, opts.query, opts.k, domain.Filter{})
	if err != nil {
		return fmt.Errorf("search: semantic: %w", err)
	}

	if err := renderSearchResults(w, resp, opts.jsonOut); err != nil {
		return err
	}

	// solov2-kxo5.7: after a successful ephemeral search, offer the
	// "keep this indexed?" prompt. No-op for tracked rows, JSON output,
	// or repos that have already been prompted. Failure to record the
	// prompt outcome is non-fatal — the search itself succeeded; we
	// log to stderr so the user knows the row stays ephemeral.
	if !opts.jsonOut && rec.Kind == "ephemeral" {
		canonical, _ := lookupCanonicalURL(ctx, pools.ReadDB, rec.RepoID)
		if canonical == "" {
			canonical = rec.RootPath
		}
		if err := runAcceptancePrompt(ctx, pools.Write, rec, canonical, defaultPromptDeps(w)); err != nil {
			fmt.Fprintf(os.Stderr, "search: acceptance prompt: %v\n", err)
		}
	}
	return nil
}

// lookupCanonicalURL fetches canonical_url for repoID; empty string on
// any read error or NULL. Used by the acceptance prompt to address the
// user with the URL they actually typed instead of the cache-tier path.
func lookupCanonicalURL(ctx context.Context, db *sql.DB, repoID string) (string, error) {
	var s sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT canonical_url FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&s); err != nil {
		return "", err
	}
	if !s.Valid {
		return "", nil
	}
	return s.String, nil
}

// resolveSearchTarget picks the repo the search will run against. The
// three input modes mirror the bead AC:
//
//   - empty arg: use the repo whose RootPath matches cwd (or the only
//     registered repo, if any). Doesn't clone or scan.
//   - a filesystem path: register if needed; subsequent runs reuse the
//     registration so the index survives.
//   - a git URL: clone to ~/.veska-search-cache/<sha-of-url>/repo on
//     first use, reuse the same dir on re-runs (AC2 — index reuse).
func resolveSearchTarget(ctx context.Context, pools *sqlite.Pools, target string, w io.Writer) (repo.Record, error) {
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return repo.Record{}, fmt.Errorf("search: getwd: %w", err)
		}
		// Empty target: only run against an already-registered repo. Auto-
		// registering cwd here is a footgun — running `veska search` from
		// /tmp or any non-git directory would otherwise cold-scan a random
		// path (solov2-bbgj). The user must explicitly pass <path> or run
		// `veska repo add` first.
		rec, err := matchByPath(ctx, pools.ReadDB, cwd)
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(cwd, ".git")); statErr != nil {
				return repo.Record{}, fmt.Errorf("search: cwd %q is not a git repository; pass <path> or cd to a registered repo", cwd)
			}
			return repo.Record{}, fmt.Errorf("search: %q is not registered; run `veska repo add %s` or pass it as <path>", cwd, cwd)
		}
		return rec, nil
	}
	if isGitURL(target) {
		// solov2-kxo5.6: URL targets route through the cache tier and
		// land as kind='ephemeral' rows so the eviction story works.
		// A canonical_url match (tracked or ephemeral) short-circuits
		// the clone — same code re-used between sessions hits the
		// warm graph instead of re-parsing.
		return ephemeralEnsureFromURL(ctx, pools, target, w)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		return findOrRegisterRepo(ctx, pools, target)
	}
	return repo.Record{}, fmt.Errorf("search: target %q is neither an existing path nor a git URL", target)
}

func findOrRegisterRepo(ctx context.Context, pools *sqlite.Pools, path string) (repo.Record, error) {
	rec, err := matchByPath(ctx, pools.ReadDB, path)
	if err == nil {
		return rec, nil
	}
	// Not registered yet — add. Subsequent runs find the existing
	// registration (AC2: reuse the index).
	id, _, addErr := repo.Add(ctx, pools.Write, path)
	if addErr != nil {
		return repo.Record{}, fmt.Errorf("search: register repo %q: %w", path, addErr)
	}
	rec, err = repo.Get(ctx, pools.ReadDB, id)
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: get newly-registered repo: %w", err)
	}
	if rec.ActiveBranch == "" {
		rec.ActiveBranch = "main"
	}
	return rec, nil
}

// ensureIndexed runs a synchronous cold-scan + embedder drain when the
// repo has no promoted SHA yet. Subsequent runs against the same repo
// short-circuit, satisfying AC2 (re-use of the existing index).
//
// The embedder is started, the pending queue drained, and the worker
// stopped — turning the daemon's long-running goroutine into a
// one-shot pass that fits the CLI's lifecycle.
func ensureIndexed(ctx context.Context, pools *sqlite.Pools, rec repo.Record, w io.Writer) error {
	if rec.LastPromotedSHA != "" {
		return nil
	}
	fmt.Fprintf(w, "search: indexing %s (first run)...\n", rec.RootPath)

	loader := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := reparserFactory(pools, loader)
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

	return drainEmbedderQueue(ctx, pools, w)
}

// drainEmbedderQueue starts an Embedder.Worker, polls the pending-refs
// count to zero (or a deadline), then stops the worker. The CLI must
// not return before vectors are populated — otherwise the search runs
// against an empty vector index and returns no hits.
func drainEmbedderQueue(ctx context.Context, pools *sqlite.Pools, w io.Writer) error {
	prov, err := buildEmbeddingProvider()
	if err != nil {
		return err
	}
	vec, err := vector.NewVectorStorage("sqlite-vec", config.DefaultVectorDir())
	if err != nil {
		return fmt.Errorf("search: open vector storage: %w", err)
	}

	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write)
	worker, err := embedder.NewWorker(refs, prov, vec,
		embedder.WithInterval(100*time.Millisecond),
	)
	if err != nil {
		return fmt.Errorf("search: build embedder worker: %w", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	worker.Start(wctx)
	defer func() {
		cancel()
		worker.Wait()
	}()

	// Poll until the queue is empty or we hit a generous wall clock.
	// The default per-tick interval is 100ms so a few thousand pending
	// rows take a few seconds. A 10-minute ceiling keeps a stuck
	// embedder (Ollama unreachable mid-drain) from wedging the CLI.
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

// buildEmbeddingProvider resolves the SAME embedder the daemon elects
// (solov2-1az) — model2vec if installed, else static-v2, or Ollama when
// VESKA_EMBEDDER=ollama — so the CLI's standalone mode embeds queries in
// the same vector space the daemon's index was built in. It uses the
// marker-free Resolve: the daemon owns the sticky election marker.
func buildEmbeddingProvider() (ports.EmbeddingProvider, error) {
	baseURL := os.Getenv("VESKA_OLLAMA_URL")
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	model := os.Getenv("VESKA_EMBED_MODEL")
	if model == "" {
		model = defaultModelName
	}
	prov, err := elect.Resolve(elect.Config{
		VeskaHome:     config.DefaultVectorDir(),
		Override:      os.Getenv("VESKA_EMBEDDER"),
		Model2VecName: "potion-code-16M",
		OllamaURL:     baseURL,
		EmbedModel:    model,
	})
	if err != nil {
		return nil, fmt.Errorf("search: resolve embedder: %w", err)
	}
	return prov, nil
}

func buildSearchService(pools *sqlite.Pools) (*search.Service, error) {
	prov, err := buildEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	vec, err := vector.NewVectorStorage("sqlite-vec", config.DefaultVectorDir())
	if err != nil {
		return nil, fmt.Errorf("search: open vector storage: %w", err)
	}
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	return search.NewService(prov, vec, nodes), nil
}

// ephemeralEnsureFromURL implements the URL-target half of `veska search`
// (solov2-kxo5.6). Steps:
//
//  1. canonicalise the URL
//  2. consult canonical_url for an existing row (tracked or ephemeral) —
//     if hit, reuse it (and bump last_accessed_at when ephemeral); no
//     clone. AC3: ephemeral hit whose cache dir vanished triggers a
//     silent re-clone instead of erroring.
//  3. otherwise clone --depth=1 into RepoCachePath, repo.Add, flip kind
//     to 'ephemeral', stamp canonical_url, touch last_accessed_at
//
// Multi-phase progress (AC2) is rendered inline: the "cloning <url>"
// banner here, git's --progress lines via the Clone helper, the
// existing "search: indexing …" line from ensureIndexed, and the
// "search: index ready" line from drainEmbedderQueue.
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
		if existing.Kind == "ephemeral" {
			// AC3: cache dir vanished (user wiped ~/.cache, manual
			// rm, etc.) → re-clone silently. Drop the stale row first
			// so the re-add doesn't trip the UNIQUE(root_path) index.
			if _, statErr := os.Stat(existing.RootPath); statErr != nil {
				_, _ = pools.Write.ExecContext(ctx,
					`DELETE FROM repos WHERE repo_id = ?`, existing.RepoID)
				// fall through to clone
			} else {
				_ = repo.TouchEphemeral(ctx, pools.Write, existing.RepoID)
				fmt.Fprintf(w, "search: reusing cached clone at %s\n", existing.RootPath)
				return existing, nil
			}
		} else {
			// tracked match — the URL points at code the user already
			// has locally registered. Skip the clone entirely; the
			// existing index is authoritative.
			fmt.Fprintf(w, "search: %s is already tracked at %s\n", canonical, existing.RootPath)
			return existing, nil
		}
	}

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

// isGitURL reports whether s should be treated as a remote git URL by the
// search command. Strings starting with a path prefix or matching an
// existing filesystem path are always paths; anything else that parses
// via repo.CanonicalURL is a URL. Mirrors looksLikeRepoURL in repo.go so
// `veska search` and `veska repo add` agree on what a URL is.
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

// searchHitView is the CLI's wire shape for one hit. It mirrors the MCP
// eng_search_semantic node DTO (snake_case) so `veska search --json` and
// the tool emit byte-identical envelopes (solov2-elt, AC3). RepoID is
// populated only by the cross-repo fanout (solov2-vm5w); single-repo
// search omits it so JSON output stays byte-identical with the daemon's.
type searchHitView struct {
	NodeID          string  `json:"node_id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	FilePath        string  `json:"file_path"`
	LineStart       int     `json:"line_start,omitempty"`
	LineEnd         int     `json:"line_end,omitempty"`
	Score           float32 `json:"score"`
	ScoreNormalized float32 `json:"score_normalized"`
	Tier            string  `json:"tier,omitempty"`
	Snippet         string  `json:"snippet,omitempty"`
	RepoID          string  `json:"repo_id,omitempty"`
}

// searchEnvelope is the {results, degraded_reasons} wrapper shared by the
// daemon-dial path (decoded from eng_search_semantic) and the in-process
// fallback (mapped from search.Response).
type searchEnvelope struct {
	Results         []searchHitView `json:"results"`
	DegradedReasons []string        `json:"degraded_reasons,omitempty"`
}

// renderSearchResults maps an in-process search.Response into the wire
// envelope and renders it.
func renderSearchResults(w io.Writer, resp search.Response, jsonOut bool) error {
	env := searchEnvelope{DegradedReasons: resp.DegradedReasons}
	env.Results = make([]searchHitView, 0, len(resp.Results))
	for _, r := range resp.Results {
		env.Results = append(env.Results, searchHitView{
			NodeID:    r.NodeID,
			Name:      r.SymbolPath,
			Kind:      r.Kind,
			FilePath:  r.FilePath,
			LineStart: r.LineStart,
			LineEnd:   r.LineEnd,
			Score:     r.Score,
			Snippet:   r.Snippet,
		})
	}
	return renderSearchEnvelope(w, env, jsonOut)
}

// renderSearchEnvelope emits the envelope as indented JSON (--json) or a
// greppable one-line-per-hit table. Results is always a non-nil slice so
// the JSON carries "results": [] on a miss (solov2-elt).
func renderSearchEnvelope(w io.Writer, env searchEnvelope, jsonOut bool) error {
	if env.Results == nil {
		env.Results = []searchHitView{}
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	}
	if len(env.Results) == 0 {
		// solov2-ffi3: a silent miss reads as broken to a new user. Hint
		// at warming embeddings when we can see the daemon's pending count;
		// otherwise print a plain "no results" so the command never exits
		// without any text feedback.
		if pending, ok := pendingEmbedsHint(); ok && pending > 0 {
			fmt.Fprintf(w, "no results (%d embeds pending — try again shortly)\n", pending)
		} else {
			fmt.Fprintln(w, "no results")
		}
		for _, d := range env.DegradedReasons {
			fmt.Fprintf(w, "[degraded: %s]\n", d)
		}
		return nil
	}
	// solov2-hl70: tier + score_normalized arrive on the DTO so the CLI
	// doesn't recompute them. The MCP / in-process path is the single
	// source of truth (internal/application/search.ScoreTier +
	// NormalizeScores). We still need the absolute top for the weak-
	// top hint below.
	var top float32
	for _, r := range env.Results {
		if r.Score > top {
			top = r.Score
		}
	}
	// solov2-vm5w: when the fanout populated RepoID, render it as a leading
	// column so the user can disambiguate hits across repos.
	multiRepo := false
	for _, r := range env.Results {
		if r.RepoID != "" {
			multiRepo = true
			break
		}
	}
	// solov2-36d5: one-line legend so the score/norm/tier columns are not
	// inscrutable to first-time users. The numbers are post-fusion RRF
	// (solov2-vee5) — meaningful only relative to other hits in this
	// query, not as absolute confidence.
	fmt.Fprintln(w, "# tier: top|strong|weak (relative to this query); score: post-fusion RRF; norm: score / top_hit_score")
	for _, r := range env.Results {
		tier := r.Tier
		if tier == "" {
			// Fallback for the in-process search path that may set
			// Score but not Tier yet. Computed in-line matches the
			// application-layer logic 1:1.
			tier = "weak"
			if top > 0 && r.Score/top >= 0.95 {
				tier = "top"
			} else if top > 0 && r.Score/top >= 0.80 {
				tier = "strong"
			}
		}
		if multiRepo {
			short := r.RepoID
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Fprintf(w, "%-12s %-8s %s:%d-%d  %s  (%s, score=%.4f norm=%.2f)\n",
				short, r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name, tier, r.Score, r.ScoreNormalized)
		} else {
			fmt.Fprintf(w, "%-8s %s:%d-%d  %s  (%s, score=%.4f norm=%.2f)\n",
				r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name, tier, r.Score, r.ScoreNormalized)
		}
	}
	// solov2-gfhq: tiers (top/strong/weak) are relative to this query's top
	// hit — a query that has no strong absolute match still gets a "top"
	// label, which reads as confidence the data can't back up. When the
	// top is below an absolute floor, append a one-liner so the user knows
	// the labels are relative and the recall is weak.
	if top > 0 && top < weakTopAbsolute {
		fmt.Fprintf(w, "note: top match score is low (%.4f) — labels are relative to this query; recall may be weak. Try refining the query.\n", top)
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s%s]\n", d, degradedReasonHint(d))
	}
	return nil
}

// weakTopAbsolute is the absolute-score floor below which a query's top hit
// is considered weak. Score is post-fusion RRF (solov2-vee5):
//
//	rank-1 in one list only  → 1/(60+1)            = 0.01639
//	rank-1 in both lists     → 2 * 1/(60+1)        = 0.03279
//
// A top below ~0.018 means even the best hit only made it into ONE
// retriever's list — the cross-corroboration signal is missing and recall
// is weak. The floor sits a hair above 0.0164 so a single-list rank-1 (the
// common small-corpus case) trips the hint and prompts the user to refine.
const weakTopAbsolute = 0.018

// degradedReasonHint maps an in-band degraded_reasons code to a one-line
// actionable hint appended to the rendered line. Empty when no hint applies,
// so the bare code is still printed (solov2-0qk5). Hints are deliberately
// short so the table layout stays readable.
func degradedReasonHint(code string) string {
	switch code {
	case "embeddings_pending":
		if pending, ok := pendingEmbedsHint(); ok && pending > 0 {
			return fmt.Sprintf(" — ~%d embeds still queued; re-run shortly for fuller recall", pending)
		}
		return " — embedder worker is still draining; re-run shortly for fuller recall"
	case "low_quality_static_embedder":
		return " — install the model2vec weights for better recall: `veska install model2vec`"
	case "no_post_registration_commits":
		return " — only populates after commits land while the repo is registered with veska"
	default:
		return ""
	}
}

// daemonSearch resolves the target repo through a running daemon and runs
// the query via eng_search_semantic. handled is false when the daemon is
// unreachable or the target repo is not yet tracked — the caller then
// falls back to the in-process clone/index/query path. This keeps the
// common "search my already-indexed repo" case on the daemon's hybrid
// pipeline and avoids a second writer on veska.db (solov2-b1q, solov2-xkm).
func daemonSearch(ctx context.Context, opts runSearchOpts) (searchEnvelope, bool, error) {
	// A git-URL target needs a clone+index pass the daemon-dial path does
	// not perform; leave it to the in-process path.
	if isGitURL(opts.target) {
		return searchEnvelope{}, false, nil
	}

	repoID, branch, ok := resolveRepoViaDaemon(ctx, opts.target)
	if !ok {
		// solov2-vm5w: when the cwd isn't part of any registered repo
		// (junior who registered repos in another dir and ran search
		// from /tmp), fan out across every registered repo instead of
		// erroring. Only fires when target is empty — explicit paths /
		// URLs still go through the in-process path so we don't change
		// their semantics.
		if opts.target == "" {
			env, fanned, ferr := daemonSearchAllRepos(ctx, opts)
			if fanned {
				return env, true, ferr
			}
		}
		return searchEnvelope{}, false, nil
	}

	k := opts.k
	if k <= 0 {
		k = 10
	}
	var env searchEnvelope
	if err := callMCP(ctx, "eng_search_semantic", map[string]any{
		"repo_id": repoID,
		"branch":  branch,
		"query":   opts.query,
		"k":       k,
	}, &env); err != nil {
		// Daemon was reachable enough to resolve the repo but the search
		// call failed — surface it rather than silently re-running a
		// divergent in-process query.
		return searchEnvelope{}, true, fmt.Errorf("search: daemon eng_search_semantic: %w", err)
	}
	return env, true, nil
}

// daemonSearchAllRepos is the cross-repo fanout invoked when target is
// empty and cwd is not part of a registered repo (solov2-vm5w). It lists
// every registered repo, runs eng_search_semantic per repo with the same
// k, merges results, re-sorts by score desc, and trims to k. fanned is
// false when the registry is empty so the caller surfaces the existing
// "not registered" error instead of a silent zero-result success.
func daemonSearchAllRepos(ctx context.Context, opts runSearchOpts) (searchEnvelope, bool, error) {
	type repoRow struct {
		RepoID       string `json:"repo_id"`
		ActiveBranch string `json:"active_branch"`
	}
	var lr struct {
		Repos []repoRow `json:"repos"`
	}
	if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
		return searchEnvelope{}, false, nil
	}
	if len(lr.Repos) == 0 {
		return searchEnvelope{}, false, nil
	}
	k := opts.k
	if k <= 0 {
		k = 10
	}
	var merged searchEnvelope
	for _, r := range lr.Repos {
		branch := branchOrMain(r.ActiveBranch)
		var env searchEnvelope
		if err := callMCP(ctx, "eng_search_semantic", map[string]any{
			"repo_id": r.RepoID,
			"branch":  branch,
			"query":   opts.query,
			"k":       k,
		}, &env); err != nil {
			// Per-repo failures must not abort the whole fanout — a stuck
			// repo would otherwise suppress every other repo's hits. Track
			// it in degraded_reasons so the user still sees something.
			merged.DegradedReasons = append(merged.DegradedReasons, fmt.Sprintf("repo %s search failed: %v", r.RepoID, err))
			continue
		}
		for _, h := range env.Results {
			h.RepoID = r.RepoID
			merged.Results = append(merged.Results, h)
		}
		merged.DegradedReasons = append(merged.DegradedReasons, env.DegradedReasons...)
	}
	// Score-desc sort across the combined set, then trim to k. The score
	// is the daemon's post-fusion RRF — same scale across repos because
	// the embedder is one process — so cross-repo comparison is sound.
	sort.SliceStable(merged.Results, func(i, j int) bool {
		return merged.Results[i].Score > merged.Results[j].Score
	})
	if len(merged.Results) > k {
		merged.Results = merged.Results[:k]
	}
	return merged, true, nil
}

// resolveRepoViaDaemon maps the search target to a (repo_id, branch) the
// daemon already tracks. An empty target resolves the cwd via
// eng_get_current_repo; a filesystem path is matched against eng_list_repos
// by canonical root. ok is false (caller falls back) when the daemon is
// down or the repo is unknown.
func resolveRepoViaDaemon(ctx context.Context, target string) (repoID, branch string, ok bool) {
	type repoRow struct {
		RepoID       string `json:"repo_id"`
		RootPath     string `json:"root_path"`
		ActiveBranch string `json:"active_branch"`
	}

	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", false
		}
		var res struct {
			Repo repoRow `json:"repo"`
		}
		if err := callMCP(ctx, "eng_get_current_repo", map[string]any{"cwd": cwd}, &res); err != nil {
			return "", "", false
		}
		if res.Repo.RepoID == "" {
			return "", "", false
		}
		return res.Repo.RepoID, branchOrMain(res.Repo.ActiveBranch), true
	}

	canonical, err := filepath.Abs(target)
	if err != nil {
		return "", "", false
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	var list struct {
		Repos []repoRow `json:"repos"`
	}
	if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &list); err != nil {
		return "", "", false
	}
	for _, r := range list.Repos {
		if r.RootPath == canonical {
			return r.RepoID, branchOrMain(r.ActiveBranch), true
		}
	}
	return "", "", false
}

// scoreTier maps a raw vector score relative to the top hit in this query
// into a human label. The thresholds are deliberately loose — the embedder's
// absolute score depends on model, query length, and corpus, so any fixed
// cut-off is wrong on some corpus. A relative tier ("the top hit is 100% of
// the top hit; this one is 88% of it") gives the user something to act on
// without pretending to be calibrated.
func scoreTier(s, top float32) string {
	if top <= 0 {
		return "weak"
	}
	ratio := s / top
	switch {
	case ratio >= 0.95:
		return "top"
	case ratio >= 0.80:
		return "strong"
	default:
		return "weak"
	}
}

// pendingEmbedsHint asks the daemon (if reachable) how many embeds are still
// queued so a zero-result search can tell the user "the index is still
// warming up" instead of staying silent. Returns ok=false if the daemon is
// down or doesn't expose the field — the caller falls back to a plain "no
// results" line (solov2-ffi3).
func pendingEmbedsHint() (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status struct {
		PendingEmbeds int `json:"pending_embeds"`
	}
	if err := callMCP(ctx, "eng_get_status", map[string]any{}, &status); err != nil {
		return 0, false
	}
	return status.PendingEmbeds, true
}

func branchOrMain(b string) string {
	if b == "" {
		return "main"
	}
	return b
}
