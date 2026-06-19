// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Dual-model fusion bench. Companion to TestEmbedModelsBenchmark
// instead of testing each model independently across corpora, this
// test embeds every doc with TWO models and compares four ranking
// strategies on the same ground-truth pairs:
//   1. code-only - rank by potion-code-16M cosine alone (baseline)
//   2. base-only - rank by potion-base-32M cosine alone (baseline)
//   3. concat - store [code_vec || base_vec] L2-normalized, rank
//                   by concat-cosine. Mathematically equivalent to
//                   the mean of the two per-model cosines.
//   4. RRF - rank docs in each model's space independently,
//                   fuse via reciprocal rank fusion
//                   (score = Σ 1/(K + rank_in_list_i), K=60 per Cormack
//                   et al. 2009).
// Output: tools/loadtest/embed_models/out/fuse-results.json and a
// fusion section appended to the published embedder-benchmarks.md.
// Run with: make eval-embed-models-fuse
// Env knobs (mirror the main bench):
//   EMBED_BENCH_MAX_DOCS - cap per corpus (default 5000)
//   FUSE_MODEL_CODE - code-side model name (default potion-code-16M)
//   FUSE_MODEL_PROSE - prose-side model name (default potion-base-32M)
//   FUSE_RRF_K - RRF constant (default 60)

package embed_models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
)

// fuseDoc holds a doc plus its TWO embeddings - the code-model vector
// and the prose-model vector. Vectors are L2-normalized by the model2vec
// adapter so dot product == cosine throughout.
type fuseDoc struct {
	name     string
	file     string
	codeVec  []float32
	proseVec []float32
}

// fuseResult is one corpus's outcome - four recall maps from the same
// fixture pairs, evaluated by four different ranking strategies.
type fuseResult struct {
	Corpus     string                  `json:"corpus"`
	Kind       string                  `json:"kind"`
	DocCount   int                     `json:"doc_count"`
	CodeModel  string                  `json:"code_model"`
	ProseModel string                  `json:"prose_model"`
	RRFK       int                     `json:"rrf_k"`
	CodeOnly   map[string]RecallScores `json:"code_only"`
	ProseOnly  map[string]RecallScores `json:"prose_only"`
	Concat     map[string]RecallScores `json:"concat"`
	RRF        map[string]RecallScores `json:"rrf"`
}

func TestEmbedModelsFusion(t *testing.T) {
	codeName := envOr("FUSE_MODEL_CODE", "potion-code-16M")
	proseName := envOr("FUSE_MODEL_PROSE", "potion-base-32M")
	rrfK := envInt("FUSE_RRF_K", 60)
	maxDocs := envInt("EMBED_BENCH_MAX_DOCS", 5000)

	codeDir := filepath.Join(modelRoot(), codeName)
	proseDir := filepath.Join(modelRoot(), proseName)
	if !fileNonEmpty(filepath.Join(codeDir, "model.safetensors")) {
		t.Skipf("code model %s not installed at %s - run scripts/install-bench-models.sh", codeName, codeDir)
	}
	if !fileNonEmpty(filepath.Join(proseDir, "model.safetensors")) {
		t.Skipf("prose model %s not installed at %s - run scripts/install-bench-models.sh", proseName, proseDir)
	}
	codeP, err := model2vec.New(codeDir)
	if err != nil {
		t.Fatalf("load %s: %v", codeName, err)
	}
	proseP, err := model2vec.New(proseDir)
	if err != nil {
		t.Fatalf("load %s: %v", proseName, err)
	}

	corpora := discoverCorpora(t)
	if len(corpora) == 0 {
		t.Fatalf("no corpora available")
	}
	t.Logf("fusion bench: code=%s prose=%s rrf_k=%d corpora=%d", codeName, proseName, rrfK, len(corpora))

	var results []fuseResult
	for _, c := range corpora {
		t.Logf("--- corpus=%s (%s) ---", c.name, c.kind)
		docs := embedFuseCorpus(t, codeP, proseP, c, maxDocs)
		if len(docs) == 0 {
			t.Logf("  no docs - skip")
			continue
		}
		gtSources := CollectGroundTruth(c.name, c.root, fixturesDir(), c.kind)
		row := fuseResult{
			Corpus:     c.name,
			Kind:       c.kind,
			DocCount:   len(docs),
			CodeModel:  codeName,
			ProseModel: proseName,
			RRFK:       rrfK,
			CodeOnly:   make(map[string]RecallScores, len(gtSources)),
			ProseOnly:  make(map[string]RecallScores, len(gtSources)),
			Concat:     make(map[string]RecallScores, len(gtSources)),
			RRF:        make(map[string]RecallScores, len(gtSources)),
		}
		for _, gt := range gtSources {
			if len(gt.Pairs) == 0 {
				continue
			}
			co, po, cc, rf := computeFusionRecall(codeP, proseP, gt.Pairs, docs, rrfK)
			row.CodeOnly[gt.Name] = co
			row.ProseOnly[gt.Name] = po
			row.Concat[gt.Name] = cc
			row.RRF[gt.Name] = rf
			t.Logf("  %-12s n=%d  code=%.3f  prose=%.3f  concat=%.3f  rrf=%.3f  (fair @10)",
				gt.Name, co.N, co.FairAt10, po.FairAt10, cc.FairAt10, rf.FairAt10)
		}
		results = append(results, row)
	}

	if err := writeFuseResults(results); err != nil {
		t.Fatalf("write fuse-results: %v", err)
	}
	if err := appendFuseSectionToMarkdown(results); err != nil {
		t.Logf("WARN: append fuse section: %v", err)
	} else {
		t.Logf("appended fusion section to docs/operations/embedder-benchmarks.md")
	}
}

// embedFuseCorpus walks the corpus and embeds each doc with BOTH
// models. Mirrors embedCorpus/embedProseCorpus but stores two vectors
// per doc. The two passes are sequential - model2vec is fully
// CPU-bound and sub-ms per embed, so parallelising adds complexity for
// no realistic speedup at this scale.
func embedFuseCorpus(t *testing.T, codeP, proseP Embedder, c corpusEntry, maxDocs int) []fuseDoc {
	t.Helper()
	// Reuse the existing single-vector loaders to get the doc set + the
	// CODE-model vectors. Then run a second pass with the prose model
	// to populate proseVec.
	var docs []doc
	switch c.kind {
	case "prose":
		docs, _, _ = embedProseCorpus(codeP, c.root, maxDocs, condenseConfig{})
	default:
		docs, _, _ = embedCorpus(t, codeP, c.root, maxDocs, condenseConfig{})
	}
	if len(docs) == 0 {
		return nil
	}
	// Re-embed each doc with the prose model. We don't have the raw
	// source text here (only the resulting vector). Re-load it from
	// disk per doc would be expensive and fragile (the chunker decides
	// the split). Instead we re-walk the corpus with the prose
	// embedder; both walkers are deterministic about ordering so the
	// resulting slices align position-for-position.
	var proseDocs []doc
	switch c.kind {
	case "prose":
		proseDocs, _, _ = embedProseCorpus(proseP, c.root, maxDocs, condenseConfig{})
	default:
		proseDocs, _, _ = embedCorpus(t, proseP, c.root, maxDocs, condenseConfig{})
	}
	if len(proseDocs) != len(docs) {
		t.Logf("  WARN: walker mismatch - code=%d prose=%d (truncating to min)", len(docs), len(proseDocs))
	}
	n := len(docs)
	if len(proseDocs) < n {
		n = len(proseDocs)
	}
	out := make([]fuseDoc, n)
	for i := 0; i < n; i++ {
		// Sanity: both walkers should produce the same (name, file)
		// pair at position i. If not, the corpus changed mid-flight or
		// the walkers diverged - fail rather than silently mis-align.
		if docs[i].name != proseDocs[i].name || docs[i].file != proseDocs[i].file {
			t.Fatalf("fuse-walker divergence at i=%d: code=(%s,%s) prose=(%s,%s)",
				i, docs[i].name, docs[i].file, proseDocs[i].name, proseDocs[i].file)
		}
		out[i] = fuseDoc{
			name:     docs[i].name,
			file:     docs[i].file,
			codeVec:  docs[i].vec,
			proseVec: proseDocs[i].vec,
		}
	}
	return out
}

// computeFusionRecall is the dual of ComputeRecall - embeds each pair's
// query with BOTH models and reports four recall series (one per
// strategy) on the same fixture set.
func computeFusionRecall(codeP, proseP Embedder, pairs []Pair, docs []fuseDoc, rrfK int) (codeOnly, proseOnly, concat, rrf RecallScores) {
	codeOnly.N, proseOnly.N, concat.N, rrf.N = len(pairs), len(pairs), len(pairs), len(pairs)
	codeOnly.Total, proseOnly.Total, concat.Total, rrf.Total = len(pairs), len(pairs), len(pairs), len(pairs)

	// Pre-compute the set of names present in the corpus so all four
	// strategies see the same NotInCorpus accounting.
	corpusNames := make(map[string]bool, len(docs))
	for _, d := range docs {
		corpusNames[d.name] = true
	}

	var (
		hitsCode1, hitsCode5, hitsCode10, mrrCode     float64
		hitsProse1, hitsProse5, hitsProse10, mrrProse float64
		hitsCat1, hitsCat5, hitsCat10, mrrCat         float64
		hitsRRF1, hitsRRF5, hitsRRF10, mrrRRF         float64
		notInCorpus, miss                             int
	)

	for _, p := range pairs {
		if !corpusNames[p.Expected] {
			notInCorpus++
			miss++
			continue
		}
		qCode, err := codeP.Embed(context.Background(), p.Query)
		if err != nil {
			miss++
			continue
		}
		qProse, err := proseP.Embed(context.Background(), p.Query)
		if err != nil {
			miss++
			continue
		}

		// Compute the four per-doc score vectors in one pass.
		type scored struct {
			idx int
			s   float64 // composite/fused score (varies by strategy)
		}
		codeScored := make([]scored, len(docs))
		proseScored := make([]scored, len(docs))
		catScored := make([]scored, len(docs))
		for i, d := range docs {
			cs := float64(dotF(qCode, d.codeVec))
			ps := float64(dotF(qProse, d.proseVec))
			codeScored[i] = scored{i, cs}
			proseScored[i] = scored{i, ps}
			// Concat-cosine is mean of the two cosines (since both
			// vectors are unit-norm and a concat of two unit-norm
			// vectors has norm sqrt(2); the renormalized concat
			// cosine evaluates to (cs+ps)/2).
			catScored[i] = scored{i, (cs + ps) / 2}
		}

		// Sort each strategy descending. Stable sort so equal-score
		// ties break by original index (deterministic).
		sortScored := func(s []scored) {
			sort.SliceStable(s, func(i, j int) bool { return s[i].s > s[j].s })
		}
		sortScored(codeScored)
		sortScored(proseScored)
		sortScored(catScored)

		// Rank-of-expected helper.
		rankOf := func(s []scored, expected string) int {
			for r, e := range s {
				if docs[e.idx].name == expected {
					return r
				}
			}
			return -1
		}
		rCode := rankOf(codeScored, p.Expected)
		rProse := rankOf(proseScored, p.Expected)
		rCat := rankOf(catScored, p.Expected)

		// RRF: per-doc score = 1/(K+rank_code) + 1/(K+rank_prose),
		// where rank is the doc's full rank in each list (0-based).
		// We have both per-doc lists fully sorted - invert each to a
		// (idx -> rank) map and combine.
		codeRankByIdx := make(map[int]int, len(docs))
		for r, e := range codeScored {
			codeRankByIdx[e.idx] = r
		}
		proseRankByIdx := make(map[int]int, len(docs))
		for r, e := range proseScored {
			proseRankByIdx[e.idx] = r
		}
		rrfScored := make([]scored, len(docs))
		for i := range docs {
			rrfScored[i] = scored{
				idx: i,
				s:   1.0/float64(rrfK+codeRankByIdx[i]) + 1.0/float64(rrfK+proseRankByIdx[i]),
			}
		}
		sortScored(rrfScored)
		rRRF := rankOf(rrfScored, p.Expected)

		tally := func(rank int, h1, h5, h10, mrr *float64) {
			if rank < 0 {
				return
			}
			if rank == 0 {
				*h1++
			}
			if rank < 5 {
				*h5++
			}
			if rank < 10 {
				*h10++
			}
			*mrr += 1.0 / float64(rank+1)
		}
		tally(rCode, &hitsCode1, &hitsCode5, &hitsCode10, &mrrCode)
		tally(rProse, &hitsProse1, &hitsProse5, &hitsProse10, &mrrProse)
		tally(rCat, &hitsCat1, &hitsCat5, &hitsCat10, &mrrCat)
		tally(rRRF, &hitsRRF1, &hitsRRF5, &hitsRRF10, &mrrRRF)
	}

	denom := float64(len(pairs))
	fairN := len(pairs) - notInCorpus
	fairDenom := float64(fairN)

	finalize := func(s *RecallScores, h1, h5, h10, mrr float64) {
		s.NotInCorpus = notInCorpus
		s.Miss = miss
		s.FairN = fairN
		s.At1 = h1 / denom
		s.At5 = h5 / denom
		s.At10 = h10 / denom
		s.MRR = mrr / denom
		if fairN > 0 {
			s.FairAt1 = h1 / fairDenom
			s.FairAt5 = h5 / fairDenom
			s.FairAt10 = h10 / fairDenom
			s.FairMRR = mrr / fairDenom
		}
	}
	finalize(&codeOnly, hitsCode1, hitsCode5, hitsCode10, mrrCode)
	finalize(&proseOnly, hitsProse1, hitsProse5, hitsProse10, mrrProse)
	finalize(&concat, hitsCat1, hitsCat5, hitsCat10, mrrCat)
	finalize(&rrf, hitsRRF1, hitsRRF5, hitsRRF10, mrrRRF)
	return
}

// dotF is the same as the bench's internal dot but kept local so the
// fusion path is self-contained (no risk of touching the main bench's
// hot path).
func dotF(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

func writeFuseResults(rows []fuseResult) error {
	out := os.Getenv("FUSE_RESULTS_OUT")
	if out == "" {
		_, file, _, _ := runtime.Caller(0)
		out = filepath.Join(filepath.Dir(file), "out", "fuse-results.json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	payload := map[string]any{
		"phase":        "8hka",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"runs":         rows,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}

// appendFuseSectionToMarkdown writes (or replaces) a "## Dual-model
// fusion" section in We don't
// regenerate the rest of the table - that's owned by writeMarkdownTable
// from the main bench. The section is delimited by start/end markers
// so re-runs replace it idempotently.
func appendFuseSectionToMarkdown(rows []fuseResult) error {
	if len(rows) == 0 {
		return nil
	}
	out := os.Getenv("EMBED_BENCH_TABLE_OUT")
	if out == "" {
		_, file, _, _ := runtime.Caller(0)
		repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
		out = filepath.Join(repoRoot, "docs", "operations", "embedder-benchmarks.md")
	}
	existing, _ := os.ReadFile(out)
	startMarker := "<!-- BEGIN FUSION-8hka -->"
	endMarker := "<!-- END FUSION-8hka -->"

	codeName := rows[0].CodeModel
	proseName := rows[0].ProseModel
	rrfK := rows[0].RRFK

	var sb strings.Builder
	sb.WriteString(startMarker + "\n")
	sb.WriteString("\n## Dual-model fusion \n\n")
	sb.WriteString("Compares four ranking strategies on the SAME headline ground-truth pairs ")
	sb.WriteString("using two model2vec variants embedded per doc:\n\n")
	sb.WriteString("- **code-only**: `" + codeName + "` cosine alone (baseline).\n")
	sb.WriteString("- **prose-only**: `" + proseName + "` cosine alone (baseline).\n")
	sb.WriteString("- **concat**: rank by mean of the two cosines (equivalent to L2-norm `[code‖prose]` concat).\n")
	fmt.Fprintf(&sb, "- **RRF**: rank docs in each model's space; fuse via `Σ 1/(K+rank)`, K=%d.\n\n", rrfK)
	sb.WriteString("Fair-R@10 on the `headline` ground-truth set per corpus:\n\n")
	sb.WriteString("| Corpus | code-only | prose-only | concat | RRF | Best vs single |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rows {
		co := r.CodeOnly["headline"]
		po := r.ProseOnly["headline"]
		cc := r.Concat["headline"]
		rf := r.RRF["headline"]
		if co.N == 0 {
			continue
		}
		best := co.FairAt10
		if po.FairAt10 > best {
			best = po.FairAt10
		}
		fuseBest := cc.FairAt10
		fuseSrc := "concat"
		if rf.FairAt10 > fuseBest {
			fuseBest = rf.FairAt10
			fuseSrc = "RRF"
		}
		delta := fuseBest - best
		mark := "-"
		if delta >= 0.05 {
			mark = "✓ " + fuseSrc + " wins"
		} else if delta <= -0.05 {
			mark = "✗ fusion loses"
		} else {
			mark = "≈ tie (" + fuseSrc + ")"
		}
		sb.WriteString("| `" + r.Corpus + "` ")
		sb.WriteString("| " + ftoa(co.FairAt10) + " ")
		sb.WriteString("| " + ftoa(po.FairAt10) + " ")
		sb.WriteString("| " + ftoa(cc.FairAt10) + " ")
		sb.WriteString("| " + ftoa(rf.FairAt10) + " ")
		sb.WriteString("| " + mark + " |\n")
	}
	sb.WriteString("\n*A corpus is counted as a fusion-win only if the best of (concat, RRF) beats the best single-model baseline by ≥+0.05 absolute R@10.*\n\n")
	sb.WriteString(endMarker + "\n")

	// Splice in / replace.
	doc := string(existing)
	if i := strings.Index(doc, startMarker); i >= 0 {
		j := strings.Index(doc, endMarker)
		if j > i {
			doc = doc[:i] + sb.String() + doc[j+len(endMarker):]
		} else {
			doc += "\n" + sb.String()
		}
	} else {
		doc += "\n" + sb.String()
	}
	return os.WriteFile(out, []byte(doc), 0o644)
}

func ftoa(v float64) string {
	if v == 0 {
		return "-"
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}
