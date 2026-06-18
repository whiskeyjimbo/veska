//go:build eval

// Package sharevsregenerate is the empirical gate for: on a real
// (large) library it measures, per derived-artifact family, the local
// REGENERATION cost against the size/sync cost of carrying that artifact in the
// shared store, and places the share-vs-regenerate line FROM DATA rather than
// assumption.
// The ADR §3 table has already made the qualitative call for four of the five
// families - parse-derived nodes/edges/FTS regenerate (cheap+deterministic);
// summaries, review, and class-B curation share (LLM-produced or irreproducible,
// content-addressed → conflict-free). The one genuinely deployment-dependent
// family is EMBEDDINGS (§5): with a cheap CPU embedder (model2vec / static) they
// may be cheaper to regenerate than to sync; with a heavy embedder (Ollama /
// nomic) they are clearly worth sharing. This harness measures that crossover.
// The verdict is expressed as a BREAKEVEN BANDWIDTH per artifact
//
//	breakeven = bytes_carried / regen_seconds
//
// the link speed at which downloading the artifact costs exactly as much as
// regenerating it locally. Below breakeven: regenerate. Above breakeven: share.
// No bandwidth is hardcoded into the verdict; reference link speeds are layered
// on top purely for display. This keeps the placed line a property of the
// measured data, with no injected free parameter.
// Runnable NOW, independent of the sharing implementation - it only times the
// existing pipeline stages (parse via the production GoParser, embed via the
// production elected embedder) and sizes the artifacts. The elected embedder is
// model2vec when installed, else the zero-dependency static-v2 hash embedder, so
// the harness runs with no external service. Point VESKA_EMBEDDER=ollama (with
// Ollama up) to capture the heavy-embedder data point and watch the embedding
// verdict flip from REGENERATE to SHARE.
// Usage:
//
//	make eval-share-vs-regenerate # internal/ self-corpus
//	SHARE_REGEN_ROOT=/path/to/kubernetes \
//	  make eval-share-vs-regenerate # a genuinely large repo
//	VESKA_EMBEDDER=ollama make eval-share-vs-regenerate # heavy-embedder crossover
package sharevsregenerate

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// resultsFile resolves RESULTS.md next to this source file, so the path
// is correct regardless of the test's working directory.
func resultsFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "RESULTS.md")
}

// refLinks are display-only reference link speeds (MB/s) the report
// evaluates each artifact's breakeven against. They never enter a
// verdict computation - the verdict is the breakeven number itself.
var refLinks = []struct {
	name string
	mbs  float64
}{
	{"1 Mbit WAN", 0.125},
	{"100 Mbit", 12.5},
	{"1 Gbit LAN", 125},
	{"10 Gbit", 1250},
}

// parseStats captures the cold-scan stage: wall time to parse every
// source file and the nodes/edges it yields. nodes carry the embed
// input text so the embed stage does not re-walk the tree.
type parseStats struct {
	wall      time.Duration
	files     int
	nodeCount int
	edgeCount int
	srcBytes  int64
	texts     []string // embed inputs (name + raw content), one per embeddable node
}

// embedStats captures the embed stage against the elected embedder.
type embedStats struct {
	model string
	wall  time.Duration
	count int
	dim   int
}

// artifactRow is one derived-artifact family's measured/decided line.
// Breakeven is 0 for families whose verdict is categorical (set by
// production cost, not by a size/time delta).
type artifactRow struct {
	Family       string  `json:"family"`
	Decided      string  `json:"decided"`        // SHARE | REGENERATE | MEASURED
	RegenSeconds float64 `json:"regen_seconds"`  // 0 when not measured here
	BytesCarried int64   `json:"bytes_carried"`  // 0 when not carried (regenerated)
	Breakeven    float64 `json:"breakeven_mb_s"` // bytes / regen; 0 when N/A
	Note         string  `json:"note"`
}

func TestShareVsRegenerate(t *testing.T) {
	root := envOr("SHARE_REGEN_ROOT", defaultRoot())
	maxDocs := envInt("SHARE_REGEN_MAX_DOCS", 5000)
	t.Logf("corpus root=%s max_docs=%d", root, maxDocs)

	ps := scanAndParse(t, root, maxDocs)
	t.Logf("parse: files=%d nodes=%d edges=%d src=%dKB wall=%s",
		ps.files, ps.nodeCount, ps.edgeCount, ps.srcBytes/1024, ps.wall.Round(time.Millisecond))
	if ps.nodeCount == 0 {
		t.Fatalf("no nodes parsed under %s - is it a Go corpus? set SHARE_REGEN_ROOT", root)
	}

	emb := embedNodes(t, ps.texts)
	t.Logf("embed: model=%s count=%d dim=%d wall=%s (per-embed mean=%s)",
		emb.model, emb.count, emb.dim, emb.wall.Round(time.Millisecond), meanPer(emb.wall, emb.count))

	rows := buildRows(ps, emb)
	report := renderReport(root, maxDocs, ps, emb, rows)
	out := resultsFile()
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
		t.Logf("WARN: write %s: %v", out, err)
	} else {
		t.Logf("results written to %s", out)
	}
	if blob, err := json.Marshal(rows); err == nil {
		fmt.Printf("SHARE_REGEN %s\n", blob)
	}
}

// scanAndParse walks every non-test.go file under root, parses each
// with the production GoParser (timing only the parse calls), and
// collects the embed input text for each node that has raw content.
func scanAndParse(t *testing.T, root string, maxDocs int) parseStats {
	t.Helper()
	parser := treesitter.NewGoParser()
	ps := parseStats{texts: make([]string, 0, maxDocs)}

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if len(ps.texts) >= maxDocs {
			return filepath.SkipAll
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		ps.srcBytes += int64(len(src))
		ps.files++
		t0 := time.Now()
		res, perr := parser.ParseFile(context.Background(), "bench", path, src)
		ps.wall += time.Since(t0)
		if perr != nil || res == nil {
			return nil
		}
		collectNodes(&ps, res, maxDocs)
		return nil
	})
	return ps
}

// collectNodes accumulates node/edge counts and embed inputs from one
// parse result, capped at maxDocs embeddable nodes.
func collectNodes(ps *parseStats, res *domain.ParseResult, maxDocs int) {
	ps.edgeCount += len(res.Edges)
	for _, n := range res.Nodes {
		ps.nodeCount++
		if len(ps.texts) >= maxDocs {
			continue
		}
		if n.RawContent == nil || *n.RawContent == "" {
			continue
		}
		ps.texts = append(ps.texts, n.Name+"\n"+*n.RawContent)
	}
}

// embedNodes resolves the production-elected embedder and times one
// Embed per collected node text. With no override this is model2vec
// (if installed) or the zero-dep static-v2 embedder, so it runs with
// no external service; VESKA_EMBEDDER=ollama swaps in the heavy path.
func embedNodes(t *testing.T, texts []string) embedStats {
	t.Helper()
	provider, err := elect.Resolve(electConfig())
	if err != nil {
		t.Fatalf("elect embedder: %v", err)
	}
	st := embedStats{model: provider.ModelID()}
	ctx := context.Background()
	if len(texts) > 0 {
		// Warmup (discarded): absorbs any one-time first-call cost so the
		// timed loop reflects steady-state per-embed latency.
		if _, werr := provider.Embed(ctx, texts[0]); werr != nil {
			t.Fatalf("embed warmup (model=%s): %v", st.model, werr)
		}
	}
	for _, text := range texts {
		t0 := time.Now()
		v, eerr := provider.Embed(ctx, text)
		st.wall += time.Since(t0)
		if eerr != nil {
			t.Fatalf("embed (model=%s): %v", st.model, eerr)
		}
		st.count++
		if st.dim == 0 {
			st.dim = len(v)
		}
	}
	return st
}

func electConfig() elect.Config {
	return elect.Config{
		VeskaHome:     veskaHome(),
		Override:      envOr("VESKA_EMBEDDER", elect.OverrideAuto),
		Model2VecName: os.Getenv("VESKA_MODEL2VEC_NAME"),
		OllamaURL:     envOr("VESKA_OLLAMA_URL", "http://localhost:11434"),
		EmbedModel:    envOr("VESKA_EMBED_MODEL", "nomic-embed-text"),
	}
}

// buildRows assembles the per-family recommendation table. Embeddings
// are measured against two vector-backend widths (memvec float32,
// usearch float16); the other families are categorical per ADR §3.
func buildRows(ps parseStats, emb embedStats) []artifactRow {
	regenS := emb.wall.Seconds()
	f32 := int64(emb.count) * int64(emb.dim) * 4
	f16 := int64(emb.count) * int64(emb.dim) * 2

	return []artifactRow{
		{
			Family:  "parse-derived (nodes/edges/FTS/imports)",
			Decided: "REGENERATE",
			Note: fmt.Sprintf("cheap + deterministic (ADR §3). %d nodes + %d edges parsed in %s; never carried.",
				ps.nodeCount, ps.edgeCount, ps.wall.Round(time.Millisecond)),
		},
		embeddingRow("embeddings (memvec / float32)", emb, f32, regenS),
		embeddingRow("embeddings (usearch / float16)", emb, f16, regenS),
		{
			Family:  "summaries / condensations",
			Decided: "SHARE",
			Note:    "LLM-produced → expensive + content-addressable (ADR §3). Measure regen cost with eval-embed-models-condense; verdict is categorical SHARE.",
		},
		{
			Family:  "review output",
			Decided: "SHARE",
			Note:    "LLM-produced → expensive + content-addressable (ADR §3). Measure regen cost with eval-review-timing; verdict is categorical SHARE.",
		},
		{
			Family:  "class-B curation (suppressions / triage)",
			Decided: "SHARE",
			Note:    "irreproducible, not source-derived (ADR §3) → always share; deterministic fold merges it.",
		},
	}
}

// embeddingRow is the decision-critical row: it reports the breakeven
// bandwidth from the measured embed wall time and the carried byte
// size. regenS is the MARGINAL embed cost - parsing is excluded because
// the graph is regenerated locally regardless, so the text is already
// in hand when deciding whether to also carry the vectors.
func embeddingRow(family string, emb embedStats, bytes int64, regenS float64) artifactRow {
	var breakeven float64
	if regenS > 0 {
		breakeven = float64(bytes) / 1e6 / regenS
	}
	return artifactRow{
		Family:       family,
		Decided:      "MEASURED",
		RegenSeconds: round3(regenS),
		BytesCarried: bytes,
		Breakeven:    round3(breakeven),
		Note: fmt.Sprintf("model=%s, %d vecs × %d dims. Below %.3g MB/s: regenerate; above: share.",
			emb.model, emb.count, emb.dim, breakeven),
	}
}

// ── report rendering ──────────────────────────────────────────────────────

func renderReport(root string, maxDocs int, ps parseStats, emb embedStats, rows []artifactRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Share-vs-Regenerate - ADR-S0019 §4 empirical gate\n\n")
	fmt.Fprintf(&b, "Generated: %s\nPlatform: %s %s\n",
		time.Now().UTC().Format("2006-01-02"), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Corpus root: `%s` (max_docs=%d)\n", root, maxDocs)
	fmt.Fprintf(&b, "Parsed: %d files, %d nodes, %d edges, ~%dKB source.\n",
		ps.files, ps.nodeCount, ps.edgeCount, ps.srcBytes/1024)
	fmt.Fprintf(&b, "Embedder: **%s** - %d vecs × %d dims, embed wall=%s (mean %s/vec).\n\n",
		emb.model, emb.count, emb.dim, emb.wall.Round(time.Millisecond), meanPer(emb.wall, emb.count))

	fmt.Fprintf(&b, "## Verdict - breakeven bandwidth per artifact\n\n")
	fmt.Fprintf(&b, "`breakeven = bytes_carried / regen_seconds`: the link speed at which "+
		"downloading the artifact costs exactly what regenerating it locally does. "+
		"Below breakeven, regenerate; above, share. No bandwidth is assumed - it is computed.\n\n")
	fmt.Fprintf(&b, "| Artifact | Decided | Regen | Carried | Breakeven | Note |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Family, r.Decided, regenCell(r), bytesCell(r.BytesCarried), breakevenCell(r.Breakeven), r.Note)
	}

	fmt.Fprintf(&b, "\n## Embedding verdict at reference link speeds\n\n")
	fmt.Fprintf(&b, "Display-only - derived from the breakeven above, not assumed.\n\n")
	fmt.Fprintf(&b, "| Artifact | %s |\n", strings.Join(refLinkHeaders(), " | "))
	fmt.Fprintf(&b, "|---|%s\n", strings.Repeat("---|", len(refLinks)))
	for _, r := range rows {
		if r.Decided != "MEASURED" {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s |\n", r.Family, strings.Join(linkVerdicts(r.Breakeven), " | "))
	}

	renderReading(&b, emb)
	return b.String()
}

// renderReading appends the data-driven interpretation + the measurement-basis
// disclosure to the report.
func renderReading(b *strings.Builder, emb embedStats) {
	fmt.Fprintf(b, "\n## Reading the result\n\n")
	fmt.Fprintf(b, "The breakeven scales with per-embed latency: a faster embedder produces a "+
		"HIGHER breakeven (you would need a faster link for sharing to win → lean regenerate); "+
		"a slower embedder produces a LOWER breakeven (almost any link beats re-embedding → "+
		"lean share). The crossover is therefore deployment-dependent - exactly the "+
		"per-deployment toggle ADR-S0019 §4 places behind `SharedArtifactStore`.\n\n")
	fmt.Fprintf(b, "This run measured **%s** at %s/vec. For the cheap-CPU endpoint, install the "+
		"fat-build default model2vec (`veska install model2vec`) - sub-millisecond/vec keeps the "+
		"breakeven high (regenerate up to a fast LAN). For the heavy endpoint, re-run with "+
		"`VESKA_EMBEDDER=ollama` (network round-trip per vec drives the breakeven toward zero, so "+
		"share wins at almost any link).\n",
		emb.model, meanPer(emb.wall, emb.count))
	fmt.Fprintf(b, "\n_Measurement basis: regen timed unbatched on a single goroutine; "+
		"production embeds via a worker pool + `BatchEmbeddingProvider`, so real regen "+
		"wall-clock is lower and the true breakeven is somewhat HIGHER (biased toward "+
		"regenerate). The breakeven is corpus-size-invariant - `count` cancels - so a "+
		"larger library buys measurement stability, not a different verdict._\n")
}

func regenCell(r artifactRow) string {
	if r.RegenSeconds == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fs", r.RegenSeconds)
}

func bytesCell(b int64) string {
	if b == 0 {
		return "-"
	}
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func breakevenCell(mbs float64) string {
	if mbs == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3g MB/s", mbs)
}

func refLinkHeaders() []string {
	out := make([]string, len(refLinks))
	for i, l := range refLinks {
		out[i] = fmt.Sprintf("%s (%.3g MB/s)", l.name, l.mbs)
	}
	return out
}

// linkVerdicts renders SHARE/REGEN for each reference link: share wins
// when the link is faster than breakeven (download beats regenerate).
func linkVerdicts(breakeven float64) []string {
	out := make([]string, len(refLinks))
	for i, l := range refLinks {
		if l.mbs >= breakeven {
			out[i] = "SHARE"
		} else {
			out[i] = "REGEN"
		}
	}
	return out
}

// ── small helpers ─────────────────────────────────────────────────────────

func skipDir(name string) bool {
	switch name {
	case "vendor", ".git", "node_modules", "out", "testdata":
		return true
	default:
		return false
	}
}

// defaultRoot resolves this repo's internal/ tree as the always-present
// self-corpus when SHARE_REGEN_ROOT is unset.
func defaultRoot() string {
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(repoRoot, "internal")
}

func veskaHome() string {
	if h := os.Getenv("VESKA_HOME"); h != "" {
		return h
	}
	return filepath.Join(os.Getenv("HOME"), ".veska")
}

func meanPer(d time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return (d / time.Duration(n)).Round(time.Microsecond)
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
