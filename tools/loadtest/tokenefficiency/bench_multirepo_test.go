//go:build eval

// Multi-repo extension of the single-repo token-efficiency harness
// (solov2-kcmo). Partitions the deterministic semantic synthcorpus
// across N repos, then for every query measures:
//
//   - veska tokens: cross-repo fanout + GLOBAL RRF (matches the MCP
//     handler shipped in solov2-bcn) -> top-K snippets
//   - grep+read tokens: the simulated filesystem walks ALL N repos
//
// This is the headline cross-repo number the wedge pitch (solov2-71xq)
// calls for. Single-repo numbers live in TestTokenEfficiency.
package tokenefficiency

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// MultiRepoResult is the JSON envelope for the multi-repo benchmark.
// Identical fields to Result plus the repo count so a caller can render
// "across N repos" in the summary line.
type MultiRepoResult struct {
	Result
	Repos int `json:"repos"`
}

// SummaryLine for the multi-repo result mentions the repo count so the
// docs blurb tells the reader the cross-repo story straight.
func (r MultiRepoResult) SummaryLine() string {
	return fmt.Sprintf(
		"Across %d repos, veska found the right code ~%.0f%% of the time, using about %.0f%% as many tokens as grep+read across the same workspace (range: %.0f–%.0f%% fewer; measured on %d queries).",
		r.Repos,
		r.MeanRecall*100,
		r.meanVeskaPctOfGrepMidpoint(),
		r.MeanSavingsLoVsGrep*100,
		r.MeanSavingsHiVsGrep*100,
		r.Queries,
	)
}

func TestTokenEfficiencyMultiRepo(t *testing.T) {
	repoCount := envInt("TOKEFF_REPOS", 5)
	nodesPerCluster := envInt("TOKEFF_NODES_PER_CLUSTER", 24)
	if repoCount < 2 {
		t.Fatalf("TOKEFF_REPOS=%d; multi-repo harness needs >= 2", repoCount)
	}
	if nodesPerCluster < 5 {
		t.Fatalf("TOKEFF_NODES_PER_CLUSTER=%d too small", nodesPerCluster)
	}
	const k = 10

	corpus := synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
	clusters := corpus.Clusters
	if clusters < repoCount {
		t.Fatalf("corpus has %d clusters; cannot split across %d repos", clusters, repoCount)
	}

	// Contiguous partition: each repo owns a slice of cluster ids. Last
	// repo absorbs any remainder so every cluster lands somewhere.
	perRepo := clusters / repoCount
	clusterToRepo := make([]int, clusters)
	for c := range clusters {
		r := c / perRepo
		if r >= repoCount {
			r = repoCount - 1
		}
		clusterToRepo[c] = r
	}
	repoIDs := make([]string, repoCount)
	for r := range repoCount {
		repoIDs[r] = fmt.Sprintf("tokeneff-multi-%d", r)
	}

	// Simulated filesystem: each repo gets a top-level prefix so grep
	// walks N distinct trees the way it would on a real workspace.
	filesByPath := make(map[string]string)
	fileNodeIDs := make(map[string][]string)
	nodesByRepo := make(map[string][]synthcorpus.SyntheticNode, repoCount)
	for _, n := range corpus.Nodes {
		r := clusterToRepo[n.Cluster]
		path := repoIDs[r] + "/" + n.FilePath
		filesByPath[path] += n.Text + "\n\n"
		fileNodeIDs[path] = append(fileNodeIDs[path], n.NodeID)
		nodesByRepo[repoIDs[r]] = append(nodesByRepo[repoIDs[r]], n)
	}
	fileTokens := make(map[string]int, len(filesByPath))
	for p, body := range filesByPath {
		n, err := CountTokens(body)
		if err != nil {
			t.Fatalf("CountTokens(%s): %v", p, err)
		}
		fileTokens[p] = n
	}

	// Single shared DB + vector store; nodes/embeddings carry their
	// repo_id so search.Service.Semantic per repo only returns the right
	// slice.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(tmpDir, "backups")})
	if err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	vstore, err := vector.NewVectorStorage(vector.BackendSQLiteVec, t.TempDir())
	if err != nil {
		t.Fatalf("vector.NewVectorStorage: %v", err)
	}

	embedder, embedderName, _, _ := loadEmbedder()
	bgCtx := context.Background()
	for _, repoID := range repoIDs {
		nodes := nodesByRepo[repoID]
		if len(nodes) == 0 {
			continue
		}
		seedNodesWithSnippets(t, db, repoID, "main", nodes)
		rows, err := embedAllNodes(bgCtx, embedder, embedderName, nodes)
		if err != nil {
			t.Fatalf("embedAllNodes(%s): %v", repoID, err)
		}
		if err := vstore.UpsertEmbeddings(bgCtx, repoID, "main", rows); err != nil {
			t.Fatalf("UpsertEmbeddings(%s): %v", repoID, err)
		}
	}

	svc, err := search.NewService(embedder, vstore, sqlite.NewNodeLookupRepo(db),
		search.WithLexicalSearcher(sqlite.NewLexicalRepo(db)),
	)
	if err != nil {
		t.Fatalf("construct search service: %v", err)
	}
	truth := corpus.TruthByCluster()

	var (
		perQuery   []PerQuery
		recalls    []float64
		veskaTok   []int
		grepLo     []int
		grepHi     []int
		loRecalls  []float64
		savingsLoV []float64
		savingsHiV []float64
	)

	for cluster, q := range corpus.CenterQueries {
		// Veska cross-repo: SemanticCandidates per repo + global RRF.
		// Mirrors runSemanticFanout in internal/infrastructure/mcp
		// (kept local so the eval harness doesn't reach into MCP).
		results, err := multiRepoFanoutSearch(bgCtx, svc, repoIDs, "main", q, k)
		if err != nil {
			t.Fatalf("multiRepoFanoutSearch(cluster %d): %v", cluster, err)
		}
		hits := make([]string, len(results))
		for i, r := range results {
			hits[i] = r.NodeID
		}
		r := RecallAtK(hits, truth[cluster], k)
		vt, err := VeskaTokens(results)
		if err != nil {
			t.Fatalf("VeskaTokens: %v", err)
		}

		// Grep walks every repo's filesystem.
		grepHits := SimulateGrepFilesWithMatches(q, filesByPath)
		lo, hi, loR, hiR := BaselineGrep(grepHits, fileTokens, fileNodeIDs, truth[cluster])

		row := PerQuery{
			Query:            q,
			Recall:           r,
			VeskaTokens:      vt,
			BaselineLoTokens: lo,
			BaselineHiTokens: hi,
			SavingsLoVsGrep:  SavingsRatio(vt, lo),
			SavingsHiVsGrep:  SavingsRatio(vt, hi),
			BaselineLoRecall: loR,
			BaselineHiRecall: hiR,
		}
		perQuery = append(perQuery, row)
		recalls = append(recalls, r)
		veskaTok = append(veskaTok, vt)
		grepLo = append(grepLo, lo)
		grepHi = append(grepHi, hi)
		loRecalls = append(loRecalls, loR)
		savingsLoV = append(savingsLoV, row.SavingsLoVsGrep)
		savingsHiV = append(savingsHiV, row.SavingsHiVsGrep)
	}

	res := MultiRepoResult{
		Repos: repoCount,
		Result: Result{
			Queries:             len(perQuery),
			K:                   k,
			Tokenizer:           EncodingName,
			MeanRecall:          Mean(recalls),
			MeanVeskaTokens:     MeanInt(veskaTok),
			MeanGrepLoTokens:    MeanInt(grepLo),
			MeanGrepHiTokens:    MeanInt(grepHi),
			MeanSavingsLoVsGrep: Mean(savingsLoV),
			MeanSavingsHiVsGrep: Mean(savingsHiV),
			MeanGrepLoRecall:    Mean(loRecalls),
			PerQuery:            perQuery,
			CorpusNote: fmt.Sprintf(
				"auto-generated semantic synthcorpus partitioned across %d repos; ground truth is by cluster construction; embedder=%s",
				repoCount, embedderName,
			),
			Embedder:  embedderName,
			Timestamp: time.Now().UTC(),
		},
	}
	convQ, rate, label := tokenPricingFromEnv()
	res.FillAbsoluteSavings(convQ, rate, label)
	if err := writeJSON("results-multirepo.json", res); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	fmt.Println(res.SummaryLine())
	fmt.Println(res.TokensLine())
	fmt.Printf("TOKENEFF_MULTI embedder=%s repos=%d queries=%d recall=%.2f veska_tok=%.0f grep_lo=%.0f grep_hi=%.0f savings=[%.0f%%, %.0f%%]\n",
		embedderName, res.Repos, res.Queries, res.MeanRecall, res.MeanVeskaTokens, res.MeanGrepLoTokens, res.MeanGrepHiTokens,
		res.MeanSavingsLoVsGrep*100, res.MeanSavingsHiVsGrep*100,
	)

	if res.MeanRecall == 0 {
		t.Fatalf("multi-repo mean recall is zero — embedder/corpus plumbing broken")
	}
}

// multiRepoFanoutSearch is the eval-side counterpart of
// internal/infrastructure/mcp.runSemanticFanout (kept inline so the
// harness doesn't import MCP). For each repo, fetches per-retriever
// ranked candidates via search.Service.SemanticCandidates, then runs a
// single global RRF over the pooled cross-repo candidate set. Returns
// up to k SearchResults sorted by global fused score desc.
func multiRepoFanoutSearch(ctx context.Context, svc *search.Service, repoIDs []string, branch, query string, k int) ([]SearchResult, error) {
	type pooled struct {
		repoID string
		cand   search.RankedCandidate
	}
	var pool []pooled
	for _, repoID := range repoIDs {
		resp, err := svc.SemanticCandidates(ctx, repoID, branch, query, k, domain.VectorFilter{})
		if err != nil {
			// Skip a degraded repo rather than aborting — matches the
			// production handler's "degrade, don't abort" policy.
			continue
		}
		for _, c := range resp.Candidates {
			pool = append(pool, pooled{repoID: repoID, cand: c})
		}
	}
	if len(pool) == 0 {
		return nil, nil
	}

	// Mirror the production cross-repo fusion (solov2-uuuk): cosine
	// when scores are present, RRF fallback otherwise.
	useCosine := false
	for _, pc := range pool {
		if pc.cand.VectorScore > 0 {
			useCosine = true
			break
		}
	}
	const rrfConstant = 60
	scores := make(map[string]float32, len(pool))
	candByKey := make(map[string]pooled, len(pool))
	for _, pc := range pool {
		key := pc.repoID + ":" + pc.cand.NodeID
		if _, exists := candByKey[key]; !exists {
			candByKey[key] = pc
		}
		if useCosine {
			if pc.cand.VectorScore > 0 {
				scores[key] = pc.cand.VectorScore
				if pc.cand.LexicalRank > 0 {
					scores[key] *= 1.05
				}
			} else if pc.cand.LexicalRank > 0 {
				scores[key] = 1.0 / float32(rrfConstant+pc.cand.LexicalRank)
			}
		} else {
			if pc.cand.VectorRank > 0 {
				scores[key] += 1.0 / float32(rrfConstant+pc.cand.VectorRank)
			}
			if pc.cand.LexicalRank > 0 {
				scores[key] += 1.0 / float32(rrfConstant+pc.cand.LexicalRank)
			}
		}
	}

	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		si, sj := scores[keys[i]], scores[keys[j]]
		if si != sj {
			return si > sj
		}
		return keys[i] < keys[j]
	})
	if len(keys) > k {
		keys = keys[:k]
	}
	out := make([]SearchResult, 0, len(keys))
	for _, key := range keys {
		pc := candByKey[key]
		out = append(out, SearchResult{NodeID: pc.cand.NodeID, Snippet: pc.cand.Snippet})
	}
	return out, nil
}

// Reference _ usage so unused-import linters stay quiet under sparse
// branches. db / strings appear in shared seed helpers via bench_test.go.
var _ = func(*sql.DB) {}
var _ = strings.Contains
