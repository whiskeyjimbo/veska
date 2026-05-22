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
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/composite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
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
		return findOrRegisterRepo(ctx, pools, cwd)
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

// buildEmbeddingProvider returns the same composite (Ollama→static)
// the daemon uses (solov2-soc), so the CLI's standalone mode produces
// vectors compatible with what the daemon would have written into
// the same index.
func buildEmbeddingProvider() (*composite.Provider, error) {
	baseURL := os.Getenv("VESKA_OLLAMA_URL")
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	model := os.Getenv("VESKA_EMBED_MODEL")
	if model == "" {
		model = defaultModelName
	}
	ollamaProv, err := ollama.New(model, ollama.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("search: ollama embedder: %w", err)
	}
	staticProv, err := embedstatic.New()
	if err != nil {
		return nil, fmt.Errorf("search: static embedder: %w", err)
	}
	prov, err := composite.New(ollamaProv, staticProv)
	if err != nil {
		return nil, fmt.Errorf("search: composite embedder: %w", err)
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

// renderSearchResults emits the response in the same JSON shape as the
// MCP eng_search_semantic tool (AC3) or a tabular fallback for human
// use. The MCP envelope is {results: [...], degraded_reasons: [...]}.
func renderSearchResults(w io.Writer, resp search.Response, jsonOut bool) error {
	envelope := struct {
		Results         []search.Result `json:"results"`
		DegradedReasons []string        `json:"degraded_reasons,omitempty"`
	}{Results: resp.Results, DegradedReasons: resp.DegradedReasons}
	if envelope.Results == nil {
		envelope.Results = []search.Result{}
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(envelope)
	}
	for _, r := range envelope.Results {
		fmt.Fprintf(w, "%-8s %s:%d-%d  %s  (score=%.4f)\n",
			r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.SymbolPath, r.Score)
	}
	for _, d := range envelope.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
	return nil
}
