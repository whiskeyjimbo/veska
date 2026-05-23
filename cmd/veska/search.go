package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	var k int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "search <query> [path-or-url]",
		Short:        "Semantic search; optionally clone+index a repo first",
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			var target string
			if len(args) == 2 {
				target = args[1]
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

	return renderSearchResults(w, resp, opts.jsonOut)
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
		local, err := cloneOrReuse(ctx, target, w)
		if err != nil {
			return repo.Record{}, err
		}
		return findOrRegisterRepo(ctx, pools, local)
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
	id, addErr := repo.Add(ctx, pools.WriteHot, path)
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

	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.WriteEmbed)
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

// cloneOrReuse keeps an on-disk cache of one-shot clones at
// ~/.veska-search-cache/<sha-of-url>/repo. AC2 (reuse) falls out of
// the deterministic path: re-running with the same URL skips the
// clone and lets ensureIndexed see the repo as already registered.
func cloneOrReuse(ctx context.Context, gitURL string, w io.Writer) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("search: homedir: %w", err)
	}
	sum := sha256.Sum256([]byte(gitURL))
	cacheDir := filepath.Join(home, ".veska-search-cache", hex.EncodeToString(sum[:])[:16])
	repoDir := filepath.Join(cacheDir, "repo")

	if _, statErr := os.Stat(filepath.Join(repoDir, ".git")); statErr == nil {
		fmt.Fprintf(w, "search: reusing cached clone at %s\n", repoDir)
		return repoDir, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("search: mkdir cache: %w", err)
	}
	fmt.Fprintf(w, "search: cloning %s -> %s\n", gitURL, repoDir)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", gitURL, repoDir)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("search: git clone: %w", err)
	}
	return repoDir, nil
}

// isGitURL is a cheap heuristic so an https://... or git@... positional
// is treated as a clone target, not a filesystem path. We accept
// anything with a scheme or the SSH-style "user@host:path" form.
func isGitURL(s string) bool {
	if strings.HasPrefix(s, "git@") {
		return true
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return true
	}
	return false
}

// searchHitView is the CLI's wire shape for one hit. It mirrors the MCP
// eng_search_semantic node DTO (snake_case) so `veska search --json` and
// the tool emit byte-identical envelopes (solov2-elt, AC3).
type searchHitView struct {
	NodeID    string  `json:"node_id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	FilePath  string  `json:"file_path"`
	LineStart int     `json:"line_start,omitempty"`
	LineEnd   int     `json:"line_end,omitempty"`
	Score     float32 `json:"score"`
	Snippet   string  `json:"snippet,omitempty"`
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
	for _, r := range env.Results {
		fmt.Fprintf(w, "%-8s %s:%d-%d  %s  (score=%.4f)\n",
			r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name, r.Score)
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
	return nil
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

func branchOrMain(b string) string {
	if b == "" {
		return "main"
	}
	return b
}
